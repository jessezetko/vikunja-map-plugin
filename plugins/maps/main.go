//go:build ignore

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
	Provider    string    `xorm:"varchar(50) not null default 'nominatim'" json:"provider"`
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

type MapsPlugin struct {
	httpClient *http.Client
}

var singleton = &MapsPlugin{
	httpClient: &http.Client{Timeout: 8 * time.Second},
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
	if viper.GetString("plugins.maps.google_api_key") != "" {
		return "google"
	}
	return "nominatim"
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
	mapsGroup.GET("/geocode", p.handleGeocodeSearch)
	mapsGroup.GET("/tasks/:taskID/location", p.handleGetTaskLocation)
	mapsGroup.POST("/tasks/:taskID/location", p.handleUpsertTaskLocation)
	mapsGroup.PUT("/tasks/:taskID/location", p.handleUpsertTaskLocation)
	mapsGroup.DELETE("/tasks/:taskID/location", p.handleDeleteTaskLocation)
	mapsGroup.GET("/projects/:projectID/locations", p.handleListProjectLocations)
}

func (p *MapsPlugin) handleGeocodeSearch(c *echo.Context) error {
	query := strings.TrimSpace(c.QueryParam("q"))
	if len(query) < 2 {
		return c.JSON(http.StatusOK, []GeocodeSuggestion{})
	}

	provider := p.getProvider()
	if provider == "google" {
		results, err := p.searchGooglePlaces(query)
		if err == nil && len(results) > 0 {
			return c.JSON(http.StatusOK, results)
		}
		if err != nil {
			log.Warningf("Google Places search failed, falling back to Nominatim: %v", err)
		}
		nomResults, nomErr := p.searchNominatim(query)
		if nomErr == nil {
			return c.JSON(http.StatusOK, nomResults)
		}
		log.Errorf("Both Google Places and Nominatim failed: %v / %v", err, nomErr)
		return echo.NewHTTPError(http.StatusBadGateway, "Location search failed")
	}

	results, err := p.searchNominatim(query)
	if err == nil && len(results) > 0 {
		return c.JSON(http.StatusOK, results)
	}
	if err != nil {
		log.Warningf("Nominatim search failed: %v", err)
	}
	if viper.GetString("plugins.maps.google_api_key") != "" {
		gResults, gErr := p.searchGooglePlaces(query)
		if gErr == nil {
			return c.JSON(http.StatusOK, gResults)
		}
		log.Errorf("Fallback to Google Places also failed: %v", gErr)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "Location search failed: "+err.Error())
	}
	return c.JSON(http.StatusOK, results)
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
	if err != nil || !canRead {
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
	if err != nil || !canUpdate {
		return echo.NewHTTPError(http.StatusForbidden, "Forbidden")
	}
	if err := task.ReadOne(s, currentUser); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "Task not found")
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
	if err != nil || !canUpdate {
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
	if err != nil || !canRead {
		return echo.NewHTTPError(http.StatusForbidden, "Forbidden")
	}

	var locations []TaskLocation
	if err := s.Table("task_locations").Where("project_id = ?", projectID).Find(&locations); err != nil {
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
	if ev.Task == nil {
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
