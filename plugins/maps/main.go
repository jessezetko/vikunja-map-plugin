//go:build ignore || test

package main

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/plugins"
	"code.vikunja.io/api/pkg/user"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/labstack/echo/v5"
	"github.com/spf13/viper"
	"src.techknowlogick.com/xormigrate"
	"xorm.io/xorm"
)

type TaskLocation struct {
	ID          int64     `xorm:"pk autoincr bigint not null" json:"id"`
	TaskID      int64     `xorm:"bigint not null unique index" json:"task_id"`
	ProjectID   int64     `xorm:"bigint not null index" json:"project_id"`
	DisplayName string    `xorm:"varchar(255) not null" json:"display_name"`
	Title       string    `xorm:"-" json:"title,omitempty"`
	Address     string    `xorm:"varchar(500) null" json:"address"`
	Latitude    float64   `xorm:"double not null" json:"latitude"`
	Longitude   float64   `xorm:"double not null" json:"longitude"`
	PlaceID     string    `xorm:"varchar(255) null" json:"place_id"`
	Provider    string    `xorm:"varchar(50) not null default 'photon'" json:"provider"`
	MapURL      string    `xorm:"varchar(1000) null" json:"map_url"`
	Created     time.Time `xorm:"created not null" json:"created"`
	Updated     time.Time `xorm:"updated not null" json:"updated"`
}

func (TaskLocation) TableName() string {
	return "task_locations"
}

type GeocodeSuggestion struct {
	PlaceID     string  `json:"place_id"`
	DisplayName string  `json:"display_name"`
	Address     string  `json:"address"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Provider    string  `json:"provider"`
	MapURL      string  `json:"map_url"`
}

type MapsConfigResponse struct {
	Provider            string `json:"provider"`
	SupportsAutocomplete bool   `json:"supports_autocomplete"`
}

func isValidCoordinate(lat, lon float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lon) || math.IsInf(lat, 0) || math.IsInf(lon, 0) {
		return false
	}
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

type cacheEntry struct {
	key         string
	suggestions []GeocodeSuggestion
	expiresAt   time.Time
	element     *list.Element
}

type geocodeCache struct {
	sync.RWMutex
	entries    map[string]*cacheEntry
	lruList    *list.List
	ttl        time.Duration
	negTTL     time.Duration
	maxEntries int
	stopChan   chan struct{}
	stopOnce   sync.Once
}

func newGeocodeCache(ttl, negTTL time.Duration, maxEntries int) *geocodeCache {
	if maxEntries <= 0 {
		maxEntries = 5000
	}
	if negTTL <= 0 {
		negTTL = 5 * time.Minute
	}
	c := &geocodeCache{
		entries:    make(map[string]*cacheEntry),
		lruList:    list.New(),
		ttl:        ttl,
		negTTL:     negTTL,
		maxEntries: maxEntries,
		stopChan:   make(chan struct{}),
	}
	go c.runEvictionLoop(10 * time.Minute)
	return c
}

func (c *geocodeCache) runEvictionLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.evictExpired()
		case <-c.stopChan:
			return
		}
	}
}

func (c *geocodeCache) evictExpired() {
	now := time.Now()
	c.Lock()
	defer c.Unlock()
	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			if v.element != nil {
				c.lruList.Remove(v.element)
			}
			delete(c.entries, k)
		}
	}
}

func (c *geocodeCache) isClosed() bool {
	select {
	case <-c.stopChan:
		return true
	default:
		return false
	}
}

func (c *geocodeCache) Close() {
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
}

func (c *geocodeCache) makeKey(provider, query string, lang ...string) string {
	l := ""
	if len(lang) > 0 {
		l = strings.ToLower(strings.TrimSpace(lang[0]))
	}
	return strings.ToLower(strings.TrimSpace(provider)) + ":" + l + ":" + strings.ToLower(strings.TrimSpace(query))
}

func (c *geocodeCache) Get(provider, query string, lang ...string) ([]GeocodeSuggestion, bool) {
	key := c.makeKey(provider, query, lang...)
	c.Lock()
	defer c.Unlock()

	entry, exists := c.entries[key]
	if !exists {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		if entry.element != nil {
			c.lruList.Remove(entry.element)
		}
		delete(c.entries, key)
		return nil, false
	}

	// Move accessed entry to front of LRU list
	if entry.element != nil {
		c.lruList.MoveToFront(entry.element)
	}
	return entry.suggestions, true
}

func (c *geocodeCache) Set(provider, query string, suggestions []GeocodeSuggestion, lang ...string) {
	key := c.makeKey(provider, query, lang...)
	now := time.Now()
	entryTTL := c.ttl
	if len(suggestions) == 0 {
		entryTTL = c.negTTL
	}

	c.Lock()
	defer c.Unlock()

	if entry, exists := c.entries[key]; exists {
		entry.suggestions = suggestions
		entry.expiresAt = now.Add(entryTTL)
		if entry.element != nil {
			c.lruList.MoveToFront(entry.element)
		}
		return
	}

	// Evict expired entries if at or over capacity
	if len(c.entries) >= c.maxEntries {
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				if v.element != nil {
					c.lruList.Remove(v.element)
				}
				delete(c.entries, k)
			}
		}
	}

	// If still at capacity, evict the least recently used element (back of LRU list)
	if len(c.entries) >= c.maxEntries && c.lruList.Len() > 0 {
		back := c.lruList.Back()
		if back != nil {
			c.lruList.Remove(back)
			if oldKey, ok := back.Value.(string); ok {
				delete(c.entries, oldKey)
			}
		}
	}

	elem := c.lruList.PushFront(key)
	c.entries[key] = &cacheEntry{
		key:         key,
		suggestions: suggestions,
		expiresAt:   now.Add(entryTTL),
		element:     elem,
	}
}

type clientBucket struct {
	tokens     float64
	lastRefill time.Time
	lastAccess time.Time
}

type clientRateLimiter struct {
	sync.Mutex
	buckets  map[string]*clientBucket
	rate     float64
	burst    float64
	stopChan chan struct{}
	stopOnce sync.Once
}

func newClientRateLimiter(rate, burst float64) *clientRateLimiter {
	l := &clientRateLimiter{
		buckets:  make(map[string]*clientBucket),
		rate:     rate,
		burst:    burst,
		stopChan: make(chan struct{}),
	}
	go l.runPruneLoop(10 * time.Minute)
	return l
}

func (l *clientRateLimiter) Allow(clientKey string) bool {
	l.Lock()
	defer l.Unlock()

	now := time.Now()
	b, exists := l.buckets[clientKey]
	if !exists {
		l.buckets[clientKey] = &clientBucket{
			tokens:     l.burst - 1.0,
			lastRefill: now,
			lastAccess: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
	b.lastRefill = now
	b.lastAccess = now

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true
	}

	return false
}

func (l *clientRateLimiter) runPruneLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.prune()
		case <-l.stopChan:
			return
		}
	}
}

func (l *clientRateLimiter) prune() {
	cutoff := time.Now().Add(-15 * time.Minute)
	l.Lock()
	defer l.Unlock()
	for k, b := range l.buckets {
		if b.lastAccess.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

func (l *clientRateLimiter) isClosed() bool {
	select {
	case <-l.stopChan:
		return true
	default:
		return false
	}
}

func (l *clientRateLimiter) Close() {
	l.stopOnce.Do(func() {
		close(l.stopChan)
	})
}

func (l *clientRateLimiter) Reconfigure(rate, burst float64) {
	l.Lock()
	defer l.Unlock()
	l.rate = rate
	l.burst = burst
	for _, b := range l.buckets {
		if b.tokens > burst {
			b.tokens = burst
		}
	}
}

func (l *clientRateLimiter) Rate() float64 {
	l.Lock()
	defer l.Unlock()
	return l.rate
}

func (l *clientRateLimiter) Burst() float64 {
	l.Lock()
	defer l.Unlock()
	return l.burst
}

var validLangRegex = regexp.MustCompile(`^[a-zA-Z]{2,5}(-[a-zA-Z0-9]{2,5})?$`)

func resolveLanguage(c *echo.Context) string {
	if lang := strings.TrimSpace(c.QueryParam("lang")); lang != "" {
		if validLangRegex.MatchString(lang) {
			return strings.ToLower(lang)
		}
	}

	if cfgLang := strings.TrimSpace(viper.GetString("plugins.maps.lang")); cfgLang != "" {
		if validLangRegex.MatchString(cfgLang) {
			return strings.ToLower(cfgLang)
		}
	}

	acceptLang := c.Request().Header.Get("Accept-Language")
	if acceptLang != "" {
		parts := strings.Split(acceptLang, ",")
		if len(parts) > 0 {
			primary := strings.TrimSpace(strings.Split(parts[0], ";")[0])
			langCode := strings.Split(primary, "-")[0]
			if validLangRegex.MatchString(langCode) {
				return strings.ToLower(langCode)
			}
		}
	}

	return ""
}

func getClientKey(c *echo.Context) string {
	if u, ok := c.Get("user").(*user.User); ok && u != nil && u.ID > 0 {
		return fmt.Sprintf("user:%d", u.ID)
	}
	ip := c.RealIP()
	if ip != "" {
		return "ip:" + ip
	}
	return "unknown"
}

type rateLimiter struct {
	sync.Mutex
	lastRequest time.Time
	interval    time.Duration
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{
		interval: interval,
	}
}

func (r *rateLimiter) Wait(ctx ...context.Context) error {
	r.Lock()
	defer r.Unlock()

	var reqCtx context.Context
	if len(ctx) > 0 {
		reqCtx = ctx[0]
	}

	now := time.Now()
	elapsed := now.Sub(r.lastRequest)
	if elapsed < r.interval {
		waitDuration := r.interval - elapsed
		if reqCtx != nil {
			timer := time.NewTimer(waitDuration)
			select {
			case <-timer.C:
			case <-reqCtx.Done():
				timer.Stop()
				return reqCtx.Err()
			}
		} else {
			time.Sleep(waitDuration)
		}
	}
	r.lastRequest = time.Now()
	return nil
}

const (
	maxResponseBodySize = 1024 * 1024 // 1 MB
	maxErrorBodySize    = 16 * 1024   // 16 KB
	maxGeocodeQueryLen  = 200
)

type MapsPlugin struct {
	httpClient       *http.Client
	cache            *geocodeCache
	nominatimLimiter *rateLimiter
	photonLimiter    *rateLimiter
	googleLimiter    *rateLimiter
	clientLimiter    *clientRateLimiter
}

var registerEventsOnce sync.Once

var singleton = &MapsPlugin{
	httpClient:       &http.Client{Timeout: 8 * time.Second},
	cache:            newGeocodeCache(24*time.Hour, 5*time.Minute, 5000),
	nominatimLimiter: newRateLimiter(1050 * time.Millisecond),
	photonLimiter:    newRateLimiter(100 * time.Millisecond),
	googleLimiter:    newRateLimiter(100 * time.Millisecond),
}

func NewPlugin() plugins.Plugin {
	return singleton
}

func NewMigrationPlugin() plugins.MigrationPlugin {
	return singleton
}

func NewAuthenticatedRouterPlugin() plugins.AuthenticatedRouterPlugin {
	return singleton
}

func (p *MapsPlugin) Name() string {
	return "maps"
}

func (p *MapsPlugin) Version() string {
	return "1.0.0"
}

func (p *MapsPlugin) isClientRateLimitEnabled() bool {
	if viper.IsSet("plugins.maps.client_rate_limit") {
		return viper.GetFloat64("plugins.maps.client_rate_limit") > 0
	}
	return true
}

func (p *MapsPlugin) getClientRateSettings() (float64, float64) {
	rate := 5.0
	burst := 15.0
	if viper.IsSet("plugins.maps.client_rate_limit") {
		if r := viper.GetFloat64("plugins.maps.client_rate_limit"); r > 0 {
			rate = r
		}
	}
	if viper.IsSet("plugins.maps.client_rate_burst") {
		if b := viper.GetFloat64("plugins.maps.client_rate_burst"); b > 0 {
			burst = b
		}
	}
	return rate, burst
}

func (p *MapsPlugin) Init() error {
	if p.cache == nil || p.cache.isClosed() {
		p.cache = newGeocodeCache(24*time.Hour, 5*time.Minute, 5000)
	}
	if p.isClientRateLimitEnabled() {
		rate, burst := p.getClientRateSettings()
		if p.clientLimiter == nil || p.clientLimiter.isClosed() {
			p.clientLimiter = newClientRateLimiter(rate, burst)
		} else {
			p.clientLimiter.Reconfigure(rate, burst)
		}
	} else {
		if p.clientLimiter != nil && !p.clientLimiter.isClosed() {
			p.clientLimiter.Close()
		}
		p.clientLimiter = nil
	}
	registerEventsOnce.Do(func() {
		events.RegisterListener((&models.TaskDeletedEvent{}).Name(), &TaskDeletedLocationCleanupListener{})
		events.RegisterListener((&models.TaskUpdatedEvent{}).Name(), &TaskUpdatedLocationSyncListener{})
		events.RegisterListener((&models.ProjectDeletedEvent{}).Name(), &ProjectDeletedLocationCleanupListener{})
	})
	log.Infof("Vikunja Maps plugin initialized (provider=%s)", p.getProvider())
	return nil
}

func (p *MapsPlugin) Shutdown() error {
	log.Infof("Vikunja Maps plugin shutting down")
	if p.cache != nil {
		p.cache.Close()
	}
	if p.clientLimiter != nil {
		p.clientLimiter.Close()
		p.clientLimiter = nil
	}
	return nil
}

func (p *MapsPlugin) getProvider() string {
	prov := strings.ToLower(strings.TrimSpace(viper.GetString("plugins.maps.geocoder_provider")))
	if prov == "google" {
		return "google"
	}
	if prov == "nominatim" {
		return "nominatim"
	}
	if prov == "photon" {
		return "photon"
	}
	if viper.GetString("plugins.maps.google_api_key") != "" {
		return "google"
	}
	return "photon"
}

func (p *MapsPlugin) Migrations() []*xormigrate.Migration {
	return []*xormigrate.Migration{
		{
			ID:          "20261018000001-create-task-locations-table",
			Description: "Create task_locations table for storing work item coordinates and place metadata",
			Migrate: func(tx *xorm.Engine) error {
				return tx.Table("task_locations").Sync2(&TaskLocation{})
			},
			Rollback: func(tx *xorm.Engine) error {
				return tx.DropTables("task_locations")
			},
		},
	}
}

func (p *MapsPlugin) RegisterAuthenticatedRoutes(g *echo.Group) {
	mapsGroup := g.Group("/maps")
	mapsGroup.GET("/config", p.handleGetConfig)
	mapsGroup.GET("/geocode", p.handleGeocodeSearch)
	mapsGroup.GET("/tasks/:taskID/location", p.handleGetTaskLocation)
	mapsGroup.POST("/tasks/:taskID/location", p.handleUpsertTaskLocation)
	mapsGroup.PUT("/tasks/:taskID/location", p.handleUpsertTaskLocation)
	mapsGroup.DELETE("/tasks/:taskID/location", p.handleDeleteTaskLocation)
	mapsGroup.GET("/projects/:projectID/locations", p.handleListProjectLocations)
}

func (p *MapsPlugin) handleGetConfig(c *echo.Context) error {
	prov := p.getProvider()
	return c.JSON(http.StatusOK, MapsConfigResponse{
		Provider:            prov,
		SupportsAutocomplete: prov != "nominatim",
	})
}

func (p *MapsPlugin) handleGeocodeSearch(c *echo.Context) error {
	query := strings.TrimSpace(c.QueryParam("q"))
	if len(query) < 2 {
		return c.JSON(http.StatusOK, []GeocodeSuggestion{})
	}
	if len(query) > maxGeocodeQueryLen {
		query = query[:maxGeocodeQueryLen]
	}

	// Per-client / per-user rate limiting
	if p.isClientRateLimitEnabled() && p.clientLimiter != nil {
		clientKey := getClientKey(c)
		if !p.clientLimiter.Allow(clientKey) {
			c.Response().Header().Set("Retry-After", "1")
			return echo.NewHTTPError(http.StatusTooManyRequests, "Geocoding rate limit exceeded. Please wait a moment before searching again.")
		}
	}

	provider := p.getProvider()
	lang := resolveLanguage(c)

	// Check cache first (partitioned by provider, lang, and query)
	if cached, found := p.cache.Get(provider, query, lang); found {
		return c.JSON(http.StatusOK, cached)
	}

	var (
		results []GeocodeSuggestion
		err     error
	)

	reqCtx := c.Request().Context()

	switch provider {
	case "google":
		results, err = p.searchGooglePlaces(query, reqCtx)
	case "nominatim":
		results, err = p.searchNominatim(query, lang, reqCtx)
	default: // "photon"
		results, err = p.searchPhoton(query, lang, reqCtx)
	}

	if err != nil {
		log.Errorf("Geocode search error with provider %s: %v", provider, err)
		return echo.NewHTTPError(http.StatusBadGateway, "Location search failed: "+err.Error())
	}

	// Cache both positive results and empty results (negative caching with shorter TTL)
	p.cache.Set(provider, query, results, lang)

	return c.JSON(http.StatusOK, results)
}

func (p *MapsPlugin) searchPhoton(query, lang string, ctx ...context.Context) ([]GeocodeSuggestion, error) {
	reqCtx := context.Background()
	if len(ctx) > 0 && ctx[0] != nil {
		reqCtx = ctx[0]
	}

	if p.photonLimiter != nil {
		if err := p.photonLimiter.Wait(reqCtx); err != nil {
			return nil, err
		}
	}

	baseURL := viper.GetString("plugins.maps.photon_url")
	if baseURL == "" {
		baseURL = "https://photon.komoot.io"
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("limit", "6")
	if lang != "" {
		q.Set("lang", lang)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(reqCtx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Vikunja-Maps-Plugin/1.0 (+https://github.com/jessezetko/vikunja-map-plugin)")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("photon returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed struct {
		Features []struct {
			Geometry struct {
				Coordinates []float64 `json:"coordinates"` // [lon, lat]
			} `json:"geometry"`
			Properties struct {
				OsmID       int64  `json:"osm_id"`
				OsmType     string `json:"osm_type"`
				Name        string `json:"name"`
				Street      string `json:"street"`
				Housenumber string `json:"housenumber"`
				Postcode    string `json:"postcode"`
				City        string `json:"city"`
				State       string `json:"state"`
				Country     string `json:"country"`
			} `json:"properties"`
		} `json:"features"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&parsed); err != nil {
		return nil, err
	}

	suggestions := make([]GeocodeSuggestion, 0, len(parsed.Features))
	for _, f := range parsed.Features {
		if len(f.Geometry.Coordinates) < 2 {
			continue
		}
		lon := f.Geometry.Coordinates[0]
		lat := f.Geometry.Coordinates[1]

		if !isValidCoordinate(lat, lon) {
			continue
		}

		displayName := strings.TrimSpace(f.Properties.Name)
		var addressParts []string
		if f.Properties.Street != "" {
			streetAddr := f.Properties.Street
			if f.Properties.Housenumber != "" {
				streetAddr += " " + f.Properties.Housenumber
			}
			addressParts = append(addressParts, streetAddr)
		}
		if f.Properties.City != "" {
			addressParts = append(addressParts, f.Properties.City)
		}
		if f.Properties.State != "" && f.Properties.State != f.Properties.City {
			addressParts = append(addressParts, f.Properties.State)
		}
		if f.Properties.Country != "" {
			addressParts = append(addressParts, f.Properties.Country)
		}

		fullAddress := strings.Join(addressParts, ", ")
		if displayName == "" {
			displayName = fullAddress
		}
		if fullAddress == "" {
			fullAddress = displayName
		}

		placeID := fmt.Sprintf("%s%d", f.Properties.OsmType, f.Properties.OsmID)
		if placeID == "" || placeID == "0" {
			placeID = fmt.Sprintf("%.6f,%.6f", lat, lon)
		}

		suggestions = append(suggestions, GeocodeSuggestion{
			PlaceID:     placeID,
			DisplayName: displayName,
			Address:     fullAddress,
			Latitude:    lat,
			Longitude:   lon,
			Lat:         lat,
			Lon:         lon,
			Provider:    "photon",
			MapURL:      fmt.Sprintf("https://www.openstreetmap.org/?mlat=%f&mlon=%f#map=16/%f/%f", lat, lon, lat, lon),
		})
	}

	return suggestions, nil
}

func (p *MapsPlugin) searchGooglePlaces(query string, ctx ...context.Context) ([]GeocodeSuggestion, error) {
	reqCtx := context.Background()
	if len(ctx) > 0 && ctx[0] != nil {
		reqCtx = ctx[0]
	}

	if p.googleLimiter != nil {
		if err := p.googleLimiter.Wait(reqCtx); err != nil {
			return nil, err
		}
	}

	apiKey := viper.GetString("plugins.maps.google_api_key")
	if apiKey == "" {
		return nil, fmt.Errorf("plugins.maps.google_api_key is not configured")
	}

	placesURL := viper.GetString("plugins.maps.google_places_url")
	if placesURL == "" {
		placesURL = "https://places.googleapis.com/v1/places:searchText"
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"textQuery":      query,
		"maxResultCount": 6,
	})
	req, err := http.NewRequestWithContext(reqCtx, "POST", placesURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", apiKey)
	req.Header.Set("X-Goog-FieldMask", "places.id,places.displayName,places.formattedAddress,places.location,places.googleMapsUri")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("google places returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed struct {
		Places []struct {
			ID          string `json:"id"`
			DisplayName struct {
				Text string `json:"text"`
			} `json:"displayName"`
			FormattedAddress string `json:"formattedAddress"`
			Location         struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"location"`
			GoogleMapsURI string `json:"googleMapsUri"`
		} `json:"places"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&parsed); err != nil {
		return nil, err
	}

	suggestions := make([]GeocodeSuggestion, 0, len(parsed.Places))
	for _, pl := range parsed.Places {
		lat := pl.Location.Latitude
		lon := pl.Location.Longitude
		if !isValidCoordinate(lat, lon) {
			continue
		}

		suggestions = append(suggestions, GeocodeSuggestion{
			PlaceID:     pl.ID,
			DisplayName: pl.DisplayName.Text,
			Address:     pl.FormattedAddress,
			Latitude:    lat,
			Longitude:   lon,
			Lat:         lat,
			Lon:         lon,
			Provider:    "google",
			MapURL:      pl.GoogleMapsURI,
		})
	}
	return suggestions, nil
}

func (p *MapsPlugin) searchNominatim(query, lang string, ctx ...context.Context) ([]GeocodeSuggestion, error) {
	reqCtx := context.Background()
	if len(ctx) > 0 && ctx[0] != nil {
		reqCtx = ctx[0]
	}

	// Nominatim Usage Policy enforcement: Rate limit to 1 req/s
	if p.nominatimLimiter != nil {
		if err := p.nominatimLimiter.Wait(reqCtx); err != nil {
			return nil, err
		}
	}

	baseURL := viper.GetString("plugins.maps.nominatim_url")
	if baseURL == "" {
		baseURL = "https://nominatim.openstreetmap.org"
	}
	userAgent := viper.GetString("plugins.maps.nominatim_user_agent")
	if userAgent == "" || strings.Contains(userAgent, "admin@example.com") {
		userAgent = "Vikunja-Maps-Plugin/1.0 (+https://github.com/jessezetko/vikunja-map-plugin)"
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/search")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("format", "jsonv2")
	q.Set("addressdetails", "1")
	q.Set("limit", "6")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(reqCtx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	} else {
		req.Header.Set("Accept-Language", "en")
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("nominatim returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var raw []struct {
		PlaceID     int64  `json:"place_id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&raw); err != nil {
		return nil, err
	}

	suggestions := make([]GeocodeSuggestion, 0, len(raw))
	for _, item := range raw {
		lat, errLat := strconv.ParseFloat(item.Lat, 64)
		lon, errLon := strconv.ParseFloat(item.Lon, 64)
		if errLat != nil || errLon != nil || !isValidCoordinate(lat, lon) {
			continue // skip invalid or malformed provider coordinates
		}
		label := item.Name
		if label == "" {
			label = strings.Split(item.DisplayName, ",")[0]
		}
		suggestions = append(suggestions, GeocodeSuggestion{
			PlaceID:     strconv.FormatInt(item.PlaceID, 10),
			DisplayName: label,
			Address:     item.DisplayName,
			Latitude:    lat,
			Longitude:   lon,
			Lat:         lat,
			Lon:         lon,
			Provider:    "nominatim",
			MapURL:      fmt.Sprintf("https://www.openstreetmap.org/?mlat=%f&mlon=%f#map=16/%f/%f", lat, lon, lat, lon),
		})
	}
	return suggestions, nil
}

func (p *MapsPlugin) handleGetTaskLocation(c *echo.Context) error {
	taskID, err := strconv.ParseInt(c.Param("taskID"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid task ID")
	}
	s := db.NewSession()
	defer s.Close()

	currentUser, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
	}
	task := &models.Task{ID: taskID}
	canRead, _, err := task.CanRead(s, currentUser)
	if err != nil {
		if models.IsErrTaskDoesNotExist(err) {
			return echo.NewHTTPError(http.StatusNotFound, "Task not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "Database error")
	}
	if !canRead {
		return echo.NewHTTPError(http.StatusForbidden, "Forbidden")
	}

	loc := &TaskLocation{}
	has, err := s.Table("task_locations").Where("task_id = ?", taskID).Get(loc)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Database error")
	}
	if !has {
		return c.JSON(http.StatusOK, nil)
	}
	loc.Title = loc.DisplayName
	return c.JSON(http.StatusOK, loc)
}

func (p *MapsPlugin) handleUpsertTaskLocation(c *echo.Context) error {
	taskID, err := strconv.ParseInt(c.Param("taskID"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid task ID")
	}
	var input TaskLocation
	if err := c.Bind(&input); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid location payload")
	}
	if strings.TrimSpace(input.DisplayName) == "" {
		input.DisplayName = strings.TrimSpace(input.Title)
	}
	if strings.TrimSpace(input.DisplayName) == "" {
		input.DisplayName = strings.TrimSpace(input.Address)
	}
	if strings.TrimSpace(input.Address) == "" {
		input.Address = input.DisplayName
	}
	if strings.TrimSpace(input.DisplayName) == "" || !isValidCoordinate(input.Latitude, input.Longitude) {
		return echo.NewHTTPError(http.StatusBadRequest, "Valid display_name or title and coordinates are required")
	}

	s := db.NewSession()
	defer s.Close()

	currentUser, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
	}
	task := &models.Task{ID: taskID}
	canUpdate, err := task.CanUpdate(s, currentUser)
	if err != nil {
		if models.IsErrTaskDoesNotExist(err) {
			return echo.NewHTTPError(http.StatusNotFound, "Task not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "Database error")
	}
	if !canUpdate {
		return echo.NewHTTPError(http.StatusForbidden, "Forbidden")
	}
	if err := task.ReadOne(s, currentUser); err != nil {
		if models.IsErrTaskDoesNotExist(err) {
			return echo.NewHTTPError(http.StatusNotFound, "Task not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to read task")
	}

	existing := &TaskLocation{}
	has, err := s.Table("task_locations").Where("task_id = ?", taskID).Get(existing)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Database error")
	}

	input.TaskID = taskID
	input.ProjectID = task.ProjectID
	if input.Provider == "" {
		input.Provider = p.getProvider()
	}
	if input.MapURL == "" {
		input.MapURL = fmt.Sprintf("https://www.openstreetmap.org/?mlat=%f&mlon=%f#map=16/%f/%f", input.Latitude, input.Longitude, input.Latitude, input.Longitude)
	}

	if has {
		input.ID = existing.ID
		_, err = s.Table("task_locations").ID(existing.ID).Cols("project_id", "display_name", "address", "latitude", "longitude", "place_id", "provider", "map_url").Update(&input)
	} else {
		_, err = s.Table("task_locations").Insert(&input)
	}
	if err != nil {
		_ = s.Rollback()
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to save location")
	}
	if err := s.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to commit location")
	}
	input.Title = input.DisplayName
	return c.JSON(http.StatusOK, input)
}

func (p *MapsPlugin) handleDeleteTaskLocation(c *echo.Context) error {
	taskID, err := strconv.ParseInt(c.Param("taskID"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid task ID")
	}
	s := db.NewSession()
	defer s.Close()

	currentUser, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
	}
	task := &models.Task{ID: taskID}
	canUpdate, err := task.CanUpdate(s, currentUser)
	if err != nil {
		if models.IsErrTaskDoesNotExist(err) {
			return echo.NewHTTPError(http.StatusNotFound, "Task not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "Database error")
	}
	if !canUpdate {
		return echo.NewHTTPError(http.StatusForbidden, "Forbidden")
	}
	_, err = s.Table("task_locations").Where("task_id = ?", taskID).Delete(&TaskLocation{})
	if err != nil {
		_ = s.Rollback()
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to delete location")
	}
	if err := s.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to commit location deletion")
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "Location deleted"})
}

func (p *MapsPlugin) handleListProjectLocations(c *echo.Context) error {
	projectID, err := strconv.ParseInt(c.Param("projectID"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid project ID")
	}
	s := db.NewSession()
	defer s.Close()

	currentUser, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
	}
	project := &models.Project{ID: projectID}
	canRead, _, err := project.CanRead(s, currentUser)
	if err != nil {
		if models.IsErrProjectDoesNotExist(err) {
			return echo.NewHTTPError(http.StatusNotFound, "Project not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "Database error")
	}
	if !canRead {
		return echo.NewHTTPError(http.StatusForbidden, "Forbidden")
	}

	locations := []TaskLocation{}
	err = s.Table("task_locations").
		Select("task_locations.*").
		Join("INNER", "tasks", "tasks.id = task_locations.task_id").
		Where("tasks.project_id = ? AND tasks.deleted_at IS NULL", projectID).
		Find(&locations)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to fetch project locations")
	}
	for i := range locations {
		locations[i].Title = locations[i].DisplayName
	}
	return c.JSON(http.StatusOK, locations)
}

type TaskDeletedLocationCleanupListener struct{}

func (l *TaskDeletedLocationCleanupListener) Name() string {
	return "maps.task.deleted.cleanup"
}

func (l *TaskDeletedLocationCleanupListener) Handle(msg *message.Message) error {
	var ev models.TaskDeletedEvent
	if err := json.Unmarshal(msg.Payload, &ev); err != nil {
		return err
	}
	if ev.Task == nil || ev.Task.ID == 0 {
		return nil
	}
	s := db.NewSession()
	defer s.Close()
	_, err := s.Table("task_locations").Where("task_id = ?", ev.Task.ID).Delete(&TaskLocation{})
	if err != nil {
		_ = s.Rollback()
		return err
	}
	return s.Commit()
}

type TaskUpdatedLocationSyncListener struct{}

func (l *TaskUpdatedLocationSyncListener) Name() string {
	return "maps.task.updated.sync"
}

func (l *TaskUpdatedLocationSyncListener) Handle(msg *message.Message) error {
	var ev models.TaskUpdatedEvent
	if err := json.Unmarshal(msg.Payload, &ev); err != nil {
		return err
	}
	if ev.Task == nil || ev.Task.ID == 0 || ev.Task.ProjectID == 0 {
		return nil
	}
	s := db.NewSession()
	defer s.Close()
	_, err := s.Table("task_locations").Where("task_id = ?", ev.Task.ID).Cols("project_id").Update(&TaskLocation{
		ProjectID: ev.Task.ProjectID,
	})
	if err != nil {
		_ = s.Rollback()
		return err
	}
	return s.Commit()
}

type ProjectDeletedLocationCleanupListener struct{}

func (l *ProjectDeletedLocationCleanupListener) Name() string {
	return "maps.project.deleted.cleanup"
}

func (l *ProjectDeletedLocationCleanupListener) Handle(msg *message.Message) error {
	var ev models.ProjectDeletedEvent
	if err := json.Unmarshal(msg.Payload, &ev); err != nil {
		return err
	}
	if ev.Project == nil || ev.Project.ID == 0 {
		return nil
	}
	s := db.NewSession()
	defer s.Close()
	_, err := s.Table("task_locations").Where("project_id = ?", ev.Project.ID).Delete(&TaskLocation{})
	if err != nil {
		_ = s.Rollback()
		return err
	}
	return s.Commit()
}
