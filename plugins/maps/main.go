//go:build ignore || test

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

type cacheEntry struct {
	suggestions []GeocodeSuggestion
	expiresAt   time.Time
}

type geocodeCache struct {
	sync.RWMutex
	entries map[string]cacheEntry
	ttl     time.Duration
}

func newGeocodeCache(ttl time.Duration) *geocodeCache {
	return &geocodeCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
	}
}

func (c *geocodeCache) Get(provider, query string) ([]GeocodeSuggestion, bool) {
	key := strings.ToLower(provider) + ":" + strings.ToLower(strings.TrimSpace(query))
	c.RLock()
	entry, exists := c.entries[key]
	c.RUnlock()

	if !exists {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		c.Lock()
		delete(c.entries, key)
		c.Unlock()
		return nil, false
	}
	return entry.suggestions, true
}

func (c *geocodeCache) Set(provider, query string, suggestions []GeocodeSuggestion) {
	key := strings.ToLower(provider) + ":" + strings.ToLower(strings.TrimSpace(query))
	c.Lock()
	c.entries[key] = cacheEntry{
		suggestions: suggestions,
		expiresAt:   time.Now().Add(c.ttl),
	}
	c.Unlock()
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

func (r *rateLimiter) Wait() {
	r.Lock()
	defer r.Unlock()

	now := time.Now()
	elapsed := now.Sub(r.lastRequest)
	if elapsed < r.interval {
		time.Sleep(r.interval - elapsed)
	}
	r.lastRequest = time.Now()
}

type MapsPlugin struct {
	httpClient       *http.Client
	cache            *geocodeCache
	nominatimLimiter *rateLimiter
}

var singleton = &MapsPlugin{
	httpClient:       &http.Client{Timeout: 8 * time.Second},
	cache:            newGeocodeCache(24 * time.Hour),
	nominatimLimiter: newRateLimiter(1050 * time.Millisecond),
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

func (p *MapsPlugin) Init() error {
	events.RegisterListener((&models.TaskDeletedEvent{}).Name(), &TaskDeletedLocationCleanupListener{})
	events.RegisterListener((&models.TaskUpdatedEvent{}).Name(), &TaskUpdatedLocationSyncListener{})
	events.RegisterListener((&models.ProjectDeletedEvent{}).Name(), &ProjectDeletedLocationCleanupListener{})
	log.Infof("Vikunja Maps plugin initialized (provider=%s)", p.getProvider())
	return nil
}

func (p *MapsPlugin) Shutdown() error {
	log.Infof("Vikunja Maps plugin shutting down")
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
		{
			ID:          "20261018000002-add-default-map-views-to-existing-projects",
			Description: "Ensure all existing projects have a default Map view",
			Migrate: func(tx *xorm.Engine) error {
				_, err := tx.Exec(`
					INSERT INTO project_views (title, project_id, view_kind, position, created, updated)
					SELECT 'Map', p.id, 4, 500, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
					FROM projects p
					WHERE NOT EXISTS (
						SELECT 1 FROM project_views pv WHERE pv.project_id = p.id AND pv.view_kind = 4
					)
				`)
				return err
			},
			Rollback: func(tx *xorm.Engine) error {
				return nil
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

	provider := p.getProvider()

	// Check 24-hour cache first
	if cached, found := p.cache.Get(provider, query); found {
		return c.JSON(http.StatusOK, cached)
	}

	var (
		results []GeocodeSuggestion
		err     error
	)

	switch provider {
	case "google":
		results, err = p.searchGooglePlaces(query)
	case "nominatim":
		results, err = p.searchNominatim(query)
	default: // "photon"
		results, err = p.searchPhoton(query)
	}

	if err != nil {
		log.Errorf("Geocode search error with provider %s: %v", provider, err)
		return echo.NewHTTPError(http.StatusBadGateway, "Location search failed: "+err.Error())
	}

	if len(results) > 0 {
		p.cache.Set(provider, query, results)
	}

	return c.JSON(http.StatusOK, results)
}

func (p *MapsPlugin) searchPhoton(query string) ([]GeocodeSuggestion, error) {
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
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
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
		bodyBytes, _ := io.ReadAll(resp.Body)
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

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	suggestions := make([]GeocodeSuggestion, 0, len(parsed.Features))
	for _, f := range parsed.Features {
		if len(f.Geometry.Coordinates) < 2 {
			continue
		}
		lon := f.Geometry.Coordinates[0]
		lat := f.Geometry.Coordinates[1]

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

func (p *MapsPlugin) searchGooglePlaces(query string) ([]GeocodeSuggestion, error) {
	apiKey := viper.GetString("plugins.maps.google_api_key")
	if apiKey == "" {
		return nil, fmt.Errorf("plugins.maps.google_api_key is not configured")
	}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"textQuery":      query,
		"maxResultCount": 6,
	})
	req, err := http.NewRequest("POST", "https://places.googleapis.com/v1/places:searchText", bytes.NewReader(reqBody))
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
		bodyBytes, _ := io.ReadAll(resp.Body)
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
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	suggestions := make([]GeocodeSuggestion, 0, len(parsed.Places))
	for _, pl := range parsed.Places {
		suggestions = append(suggestions, GeocodeSuggestion{
			PlaceID:     pl.ID,
			DisplayName: pl.DisplayName.Text,
			Address:     pl.FormattedAddress,
			Latitude:    pl.Location.Latitude,
			Longitude:   pl.Location.Longitude,
			Lat:         pl.Location.Latitude,
			Lon:         pl.Location.Longitude,
			Provider:    "google",
			MapURL:      pl.GoogleMapsURI,
		})
	}
	return suggestions, nil
}

func (p *MapsPlugin) searchNominatim(query string) ([]GeocodeSuggestion, error) {
	// Nominatim Usage Policy enforcement: Rate limit to 1 req/s
	p.nominatimLimiter.Wait()

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

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("nominatim returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var raw []struct {
		PlaceID     int64  `json:"place_id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	suggestions := make([]GeocodeSuggestion, 0, len(raw))
	for _, item := range raw {
		lat, _ := strconv.ParseFloat(item.Lat, 64)
		lon, _ := strconv.ParseFloat(item.Lon, 64)
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
	if strings.TrimSpace(input.DisplayName) == "" || input.Latitude < -90 || input.Latitude > 90 || input.Longitude < -180 || input.Longitude > 180 {
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
