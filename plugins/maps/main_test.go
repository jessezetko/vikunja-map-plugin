//go:build test

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/modules/keyvalue"
	"code.vikunja.io/api/pkg/user"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/labstack/echo/v5"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	config.InitDefaultConfig()
	keyvalue.InitStorage()
	x, _ := db.CreateTestEngine()
	_ = x.Table("users").Sync2(&user.User{})
	tables := append(models.GetTables(), &TaskLocation{})
	_ = x.Sync2(tables...)
	os.Exit(m.Run())
}

func TestGeocodeCache(t *testing.T) {
	cache := newGeocodeCache(100 * time.Millisecond)

	sample := []GeocodeSuggestion{
		{
			PlaceID:     "123",
			DisplayName: "Eiffel Tower",
			Address:     "Champ de Mars, Paris",
			Latitude:    48.8584,
			Longitude:   2.2945,
			Provider:    "photon",
		},
	}

	// Miss initially
	_, found := cache.Get("photon", "eiffel")
	assert.False(t, found)

	// Set and hit (case-insensitive and trimmed)
	cache.Set("photon", "  EIFFEL  ", sample)
	cached, found := cache.Get("photon", "eiffel")
	assert.True(t, found)
	require.Len(t, cached, 1)
	assert.Equal(t, "Eiffel Tower", cached[0].DisplayName)

	// Different provider does not collide
	_, found = cache.Get("nominatim", "eiffel")
	assert.False(t, found)

	// Wait for TTL expiration
	time.Sleep(120 * time.Millisecond)
	_, found = cache.Get("photon", "eiffel")
	assert.False(t, found, "expected entry to expire after TTL")
}

func TestNominatimRateLimiter(t *testing.T) {
	interval := 50 * time.Millisecond
	limiter := newRateLimiter(interval)

	start := time.Now()
	limiter.Wait()
	limiter.Wait()
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, interval, "limiter should enforce at least interval delay between calls")
}

func TestSearchPhotonParsing(t *testing.T) {
	fakePhotonResponse := `{
		"type": "FeatureCollection",
		"features": [
			{
				"type": "Feature",
				"geometry": {
					"coordinates": [2.2945, 48.8584],
					"type": "Point"
				},
				"properties": {
					"osm_id": 5013364,
					"osm_type": "W",
					"name": "Tour Eiffel",
					"street": "Avenue Anatole France",
					"housenumber": "5",
					"city": "Paris",
					"country": "France"
				}
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/", r.URL.Path)
		assert.Equal(t, "eiffel", r.URL.Query().Get("q"))
		assert.Equal(t, "6", r.URL.Query().Get("limit"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakePhotonResponse))
	}))
	defer server.Close()

	viper.Set("plugins.maps.photon_url", server.URL)
	defer viper.Set("plugins.maps.photon_url", "")

	plugin := &MapsPlugin{
		httpClient:       server.Client(),
		cache:            newGeocodeCache(time.Minute),
		nominatimLimiter: newRateLimiter(time.Millisecond),
	}

	results, err := plugin.searchPhoton("eiffel")
	require.NoError(t, err)
	require.Len(t, results, 1)

	res := results[0]
	assert.Equal(t, "W5013364", res.PlaceID)
	assert.Equal(t, "Tour Eiffel", res.DisplayName)
	assert.Contains(t, res.Address, "Paris")
	assert.Contains(t, res.Address, "France")
	assert.InDelta(t, 48.8584, res.Latitude, 0.0001)
	assert.InDelta(t, 2.2945, res.Longitude, 0.0001)
	assert.Equal(t, "photon", res.Provider)
}

func TestSearchNominatimParsing(t *testing.T) {
	fakeNominatimResponse := `[
		{
			"place_id": 98765,
			"name": "Big Ben",
			"display_name": "Big Ben, Westminster, London, UK",
			"lat": "51.5007",
			"lon": "-0.1246"
		}
	]`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/search", r.URL.Path)
		assert.Equal(t, "big ben", r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeNominatimResponse))
	}))
	defer server.Close()

	viper.Set("plugins.maps.nominatim_url", server.URL)
	defer viper.Set("plugins.maps.nominatim_url", "")

	plugin := &MapsPlugin{
		httpClient:       server.Client(),
		cache:            newGeocodeCache(time.Minute),
		nominatimLimiter: newRateLimiter(time.Millisecond),
	}

	results, err := plugin.searchNominatim("big ben")
	require.NoError(t, err)
	require.Len(t, results, 1)

	res := results[0]
	assert.Equal(t, "98765", res.PlaceID)
	assert.Equal(t, "Big Ben", res.DisplayName)
	assert.Equal(t, "Big Ben, Westminster, London, UK", res.Address)
	assert.InDelta(t, 51.5007, res.Latitude, 0.0001)
	assert.InDelta(t, -0.1246, res.Longitude, 0.0001)
	assert.Equal(t, "nominatim", res.Provider)
}

func TestGetProviderConfig(t *testing.T) {
	plugin := &MapsPlugin{}

	// Default is photon
	viper.Set("plugins.maps.geocoder_provider", "")
	viper.Set("plugins.maps.google_api_key", "")
	assert.Equal(t, "photon", plugin.getProvider())

	// Explicit nominatim
	viper.Set("plugins.maps.geocoder_provider", "nominatim")
	assert.Equal(t, "nominatim", plugin.getProvider())

	// Explicit google
	viper.Set("plugins.maps.geocoder_provider", "google")
	assert.Equal(t, "google", plugin.getProvider())

	// Reset
	viper.Set("plugins.maps.geocoder_provider", "")
}

func TestConfigHandler(t *testing.T) {
	plugin := &MapsPlugin{}
	e := echo.New()

	// 1. Photon mode
	viper.Set("plugins.maps.geocoder_provider", "photon")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/config", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := plugin.handleGetConfig(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var conf MapsConfigResponse
	err = json.Unmarshal(rec.Body.Bytes(), &conf)
	require.NoError(t, err)
	assert.Equal(t, "photon", conf.Provider)
	assert.True(t, conf.SupportsAutocomplete)

	// 2. Nominatim mode (autocomplete prohibited)
	viper.Set("plugins.maps.geocoder_provider", "nominatim")
	rec = httptest.NewRecorder()
	c = e.NewContext(req, rec)
	err = plugin.handleGetConfig(c)
	require.NoError(t, err)

	err = json.Unmarshal(rec.Body.Bytes(), &conf)
	require.NoError(t, err)
	assert.Equal(t, "nominatim", conf.Provider)
	assert.False(t, conf.SupportsAutocomplete, "Nominatim should disallow search-as-you-type")

	viper.Set("plugins.maps.geocoder_provider", "")
}

func TestTaskMoveAndProjectCleanupListeners(t *testing.T) {
	// Initialize test db session
	s := db.NewSession()
	defer s.Close()

	// Ensure table exists
	err := s.Table("task_locations").Sync2(&TaskLocation{})
	require.NoError(t, err)

	defer func() {
		sClean := db.NewSession()
		defer sClean.Close()
		_, _ = sClean.Table("task_locations").Where("task_id IN (?, ?)", 9901, 9902).Delete(&TaskLocation{})
		_ = sClean.Commit()
	}()

	// Insert test task location in Project 100
	loc1 := &TaskLocation{
		TaskID:      9901,
		ProjectID:   100,
		DisplayName: "Project 100 Task Location",
		Latitude:    10.0,
		Longitude:   20.0,
		Provider:    "photon",
	}
	_, err = s.Table("task_locations").Insert(loc1)
	require.NoError(t, err)

	loc2 := &TaskLocation{
		TaskID:      9902,
		ProjectID:   100,
		DisplayName: "Project 100 Task Location 2",
		Latitude:    11.0,
		Longitude:   21.0,
		Provider:    "photon",
	}
	_, err = s.Table("task_locations").Insert(loc2)
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	s.Close()

	// 1. Move Task 9901 to Project 200 via TaskUpdatedLocationSyncListener
	moveEvent := &models.TaskUpdatedEvent{
		Task: &models.Task{
			ID:        9901,
			ProjectID: 200,
		},
		Doer: &user.User{ID: 1},
	}
	payload, _ := json.Marshal(moveEvent)
	msg := message.NewMessage("test-msg-1", payload)

	syncListener := &TaskUpdatedLocationSyncListener{}
	err = syncListener.Handle(msg)
	require.NoError(t, err)

	// Verify task 9901 is now under Project 200
	sVerify := db.NewSession()
	checkLoc := &TaskLocation{}
	has, err := sVerify.Table("task_locations").Where("task_id = ?", 9901).Get(checkLoc)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, int64(200), checkLoc.ProjectID, "Task location should sync to new project_id")

	// Task 9902 remains in Project 100
	checkLoc2 := &TaskLocation{}
	has, err = sVerify.Table("task_locations").Where("task_id = ?", 9902).Get(checkLoc2)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, int64(100), checkLoc2.ProjectID)
	sVerify.Close()

	// 2. Delete Project 100 via ProjectDeletedLocationCleanupListener
	projDeleteEvent := &models.ProjectDeletedEvent{
		Project: &models.Project{
			ID: 100,
		},
		Doer: &user.User{ID: 1},
	}
	payload, _ = json.Marshal(projDeleteEvent)
	msg = message.NewMessage("test-msg-2", payload)

	cleanListener := &ProjectDeletedLocationCleanupListener{}
	err = cleanListener.Handle(msg)
	require.NoError(t, err)

	// Task 9902 in Project 100 should now be deleted
	sVerify2 := db.NewSession()
	checkLoc3 := &TaskLocation{}
	has, err = sVerify2.Table("task_locations").Where("task_id = ?", 9902).Get(checkLoc3)
	require.NoError(t, err)
	assert.False(t, has, "Task location in deleted project should be removed")

	// Task 9901 in Project 200 should still exist
	checkLoc4 := &TaskLocation{}
	has, err = sVerify2.Table("task_locations").Where("task_id = ?", 9901).Get(checkLoc4)
	require.NoError(t, err)
	assert.True(t, has, "Task location in other project should remain intact")
	sVerify2.Close()
}

func TestDynamicProjectLocationsJoin(t *testing.T) {
	s := db.NewSession()
	defer s.Close()

	_ = s.Table("tasks").Sync2(&models.Task{})
	_ = s.Table("task_locations").Sync2(&TaskLocation{})

	defer func() {
		sClean := db.NewSession()
		defer sClean.Close()
		_, _ = sClean.Table("tasks").Where("id IN (?, ?)", 8801, 8802).Delete(&models.Task{})
		_, _ = sClean.Table("task_locations").Where("task_id IN (?, ?)", 8801, 8802).Delete(&TaskLocation{})
		_ = sClean.Commit()
	}()

	// Create task 8801 in project 500
	task1 := &models.Task{
		ID:        8801,
		ProjectID: 500,
		Title:     "Active Task in Project 500",
	}
	_, err := s.Table("tasks").Insert(task1)
	require.NoError(t, err)

	loc1 := &TaskLocation{
		TaskID:      8801,
		ProjectID:   500,
		DisplayName: "Location 8801",
		Latitude:    40.7128,
		Longitude:   -74.0060,
		Provider:    "photon",
	}
	_, err = s.Table("task_locations").Insert(loc1)
	require.NoError(t, err)

	// Create task 8802 originally saved with project_id=500 in task_locations,
	// but task.project_id is actually 600 (stale location record simulation)
	task2 := &models.Task{
		ID:        8802,
		ProjectID: 600,
		Title:     "Task moved to Project 600",
	}
	_, err = s.Table("tasks").Insert(task2)
	require.NoError(t, err)

	loc2 := &TaskLocation{
		TaskID:      8802,
		ProjectID:   500, // Stale!
		DisplayName: "Location 8802",
		Latitude:    34.0522,
		Longitude:   -118.2437,
		Provider:    "photon",
	}
	_, err = s.Table("task_locations").Insert(loc2)
	require.NoError(t, err)
	require.NoError(t, s.Commit())

	// Query Project 500 locations using the dynamic INNER JOIN
	sQuery := db.NewSession()
	defer sQuery.Close()

	var p500Locations []TaskLocation
	err = sQuery.Table("task_locations").
		Select("task_locations.*").
		Join("INNER", "tasks", "tasks.id = task_locations.task_id").
		Where("tasks.project_id = ? AND tasks.deleted_at IS NULL", 500).
		Find(&p500Locations)
	require.NoError(t, err)

	// Only task 8801 should be returned, not task 8802!
	assert.Len(t, p500Locations, 1)
	assert.Equal(t, int64(8801), p500Locations[0].TaskID)

	// Query Project 600 locations
	var p600Locations []TaskLocation
	err = sQuery.Table("task_locations").
		Select("task_locations.*").
		Join("INNER", "tasks", "tasks.id = task_locations.task_id").
		Where("tasks.project_id = ? AND tasks.deleted_at IS NULL", 600).
		Find(&p600Locations)
	require.NoError(t, err)

	assert.Len(t, p600Locations, 1)
	assert.Equal(t, int64(8802), p600Locations[0].TaskID)
}

func TestTaskNotFoundReturns404(t *testing.T) {
	s := db.NewSession()
	_ = s.Table("users").Sync2(&user.User{})
	u := &user.User{ID: 1, Username: "testuser"}
	_, _ = s.Table("users").Insert(u)
	_ = s.Commit()
	s.Close()

	plugin := &MapsPlugin{}
	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("api_user", u)
			return next(c)
		}
	})
	plugin.RegisterAuthenticatedRoutes(e.Group("/api/v1/plugins"))

	// GET non-existent task returns 404
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/tasks/999999/location", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// DELETE non-existent task returns 404
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/maps/tasks/999999/location", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// GET non-existent project returns 404 (not 403)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/projects/999999/locations", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "Non-existent project should return 404 Not Found, not 403")
}

func TestEmptyProjectReturnsEmptyArray(t *testing.T) {
	s := db.NewSession()
	_ = s.Table("users").Sync2(&user.User{})
	_ = s.Table("projects").Sync2(&models.Project{})
	_ = s.Table("tasks").Sync2(&models.Task{})
	_ = s.Table("task_locations").Sync2(&TaskLocation{})
	_ = s.Table("users_projects").Sync2(&models.ProjectUser{})

	u := &user.User{ID: 10, Username: "projowner"}
	_, _ = s.Table("users").Insert(u)

	p := &models.Project{ID: 7777, Title: "Empty Project", OwnerID: u.ID}
	_, _ = s.Table("projects").Insert(p)

	up := &models.ProjectUser{UserID: u.ID, ProjectID: p.ID, Permission: models.PermissionAdmin}
	_, _ = s.Table("users_projects").Insert(up)

	_, _ = s.Table("project_ancestors").Insert(map[string]interface{}{
		"project_id":  p.ID,
		"ancestor_id": p.ID,
		"depth":       0,
	})

	_ = s.Commit()
	s.Close()

	defer func() {
		sClean := db.NewSession()
		defer sClean.Close()
		_, _ = sClean.Table("projects").Where("id = ?", 7777).Delete(&models.Project{})
		_, _ = sClean.Table("users_projects").Where("project_id = ?", 7777).Delete(&models.ProjectUser{})
		_, _ = sClean.Table("project_ancestors").Where("project_id = ?", 7777).Delete(map[string]interface{}{})
		_, _ = sClean.Table("users").Where("id = ?", 10).Delete(&user.User{})
		_ = sClean.Commit()
	}()

	plugin := &MapsPlugin{}
	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("api_user", u)
			return next(c)
		}
	})
	plugin.RegisterAuthenticatedRoutes(e.Group("/api/v1/plugins"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/projects/7777/locations", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	// Verify that the body is [] and not null
	body := strings.TrimSpace(rec.Body.String())
	assert.Equal(t, "[]", body, "Empty project should return [] not null")
}

func TestGeocodeZeroResultsNoCrash(t *testing.T) {
	fakePhotonEmpty := `{"type":"FeatureCollection","features":[]}`
	calledNominatim := false

	photonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakePhotonEmpty))
	}))
	defer photonServer.Close()

	nomServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledNominatim = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	defer nomServer.Close()

	viper.Set("plugins.maps.geocoder_provider", "photon")
	viper.Set("plugins.maps.photon_url", photonServer.URL)
	viper.Set("plugins.maps.nominatim_url", nomServer.URL)
	defer func() {
		viper.Set("plugins.maps.geocoder_provider", "")
		viper.Set("plugins.maps.photon_url", "")
		viper.Set("plugins.maps.nominatim_url", "")
	}()

	plugin := &MapsPlugin{
		httpClient:       photonServer.Client(),
		cache:            newGeocodeCache(time.Minute),
		nominatimLimiter: newRateLimiter(time.Millisecond),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/geocode?q=nowhere", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := plugin.handleGeocodeSearch(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
	assert.False(t, calledNominatim, "Photon search should never leak autocomplete requests to Nominatim")
}

