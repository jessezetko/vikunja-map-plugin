//go:build test

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/log"
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
	log.InitLogger()
	keyvalue.InitStorage()
	x, _ := db.CreateTestEngine()
	_ = x.Table("users").Sync2(&user.User{})
	tables := append(models.GetTables(), &TaskLocation{})
	_ = x.Sync2(tables...)
	os.Exit(m.Run())
}

func TestGeocodeCache(t *testing.T) {
	cache := newGeocodeCache(100*time.Millisecond, 50*time.Millisecond, 3)
	defer cache.Close()

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

	// Negative result caching with shorter TTL
	cache.Set("photon", "nonexistent-query", []GeocodeSuggestion{})
	cachedEmpty, foundEmpty := cache.Get("photon", "nonexistent-query")
	assert.True(t, foundEmpty)
	assert.Empty(t, cachedEmpty)

	// Wait for negative TTL (50ms) to expire
	time.Sleep(60 * time.Millisecond)
	_, foundEmpty = cache.Get("photon", "nonexistent-query")
	assert.False(t, foundEmpty, "negative cache entry should expire after negTTL")

	// Verify positive entry still alive before its 100ms TTL
	// Wait remaining time for 100ms TTL expiration
	time.Sleep(60 * time.Millisecond)
	_, found = cache.Get("photon", "eiffel")
	assert.False(t, found, "expected entry to expire after TTL")

	// Test cache capacity cap
	cache.Set("p", "q1", sample)
	cache.Set("p", "q2", sample)
	cache.Set("p", "q3", sample)
	cache.Set("p", "q4", sample) // exceeds capacity 3, should evict
	cache.Lock()
	entryCount := len(cache.entries)
	cache.Unlock()
	assert.LessOrEqual(t, entryCount, 3, "cache should not exceed maxEntries")
}

func TestGeocodeCacheLRUEviction(t *testing.T) {
	cache := newGeocodeCache(10*time.Minute, 5*time.Minute, 3)
	defer cache.Close()

	s1 := []GeocodeSuggestion{{DisplayName: "Item 1"}}
	s2 := []GeocodeSuggestion{{DisplayName: "Item 2"}}
	s3 := []GeocodeSuggestion{{DisplayName: "Item 3"}}
	s4 := []GeocodeSuggestion{{DisplayName: "Item 4"}}

	// Add 3 entries to reach capacity: insertion order 1, 2, 3
	cache.Set("p", "q1", s1)
	cache.Set("p", "q2", s2)
	cache.Set("p", "q3", s3)

	// Access q1, which moves q1 to the front of LRU.
	// Order from MRU to LRU is now: q1, q3, q2. Thus q2 is LRU.
	cached, found := cache.Get("p", "q1")
	assert.True(t, found)
	require.Len(t, cached, 1)

	// Insert q4, which should evict LRU (q2)
	cache.Set("p", "q4", s4)

	// q1 should still be present
	_, found = cache.Get("p", "q1")
	assert.True(t, found, "recently accessed q1 should NOT be evicted")

	// q2 should have been evicted
	_, found = cache.Get("p", "q2")
	assert.False(t, found, "least recently used q2 should have been evicted")

	// q3 and q4 should be present
	_, found = cache.Get("p", "q3")
	assert.True(t, found, "q3 should still be in cache")
	_, found = cache.Get("p", "q4")
	assert.True(t, found, "q4 should be in cache")
}

func TestGeocodeCacheLanguagePartitioning(t *testing.T) {
	cache := newGeocodeCache(10*time.Minute, 5*time.Minute, 100)
	defer cache.Close()

	enResult := []GeocodeSuggestion{{DisplayName: "Munich, Germany"}}
	deResult := []GeocodeSuggestion{{DisplayName: "München, Deutschland"}}

	cache.Set("photon", "munich", enResult, "en")
	cache.Set("photon", "munich", deResult, "de")

	cachedEn, foundEn := cache.Get("photon", "munich", "en")
	assert.True(t, foundEn)
	require.Len(t, cachedEn, 1)
	assert.Equal(t, "Munich, Germany", cachedEn[0].DisplayName)

	cachedDe, foundDe := cache.Get("photon", "munich", "de")
	assert.True(t, foundDe)
	require.Len(t, cachedDe, 1)
	assert.Equal(t, "München, Deutschland", cachedDe[0].DisplayName)

	// Unspecified/default lang should not collide
	_, foundDefault := cache.Get("photon", "munich")
	assert.False(t, foundDefault)
}

func TestClientRateLimiter(t *testing.T) {
	// 5 tokens/sec, burst capacity of 3
	limiter := newClientRateLimiter(5.0, 3.0)
	defer limiter.Close()

	client := "user:42"

	// Initial burst of 3 should all succeed
	assert.True(t, limiter.Allow(client))
	assert.True(t, limiter.Allow(client))
	assert.True(t, limiter.Allow(client))

	// 4th request immediately should exceed burst and fail
	assert.False(t, limiter.Allow(client), "request exceeding burst must be throttled")

	// Independent client should not be affected
	otherClient := "user:99"
	assert.True(t, limiter.Allow(otherClient), "different client must have its own token bucket")

	// Wait for token refill (250ms at 5 req/s refills > 1 token)
	time.Sleep(250 * time.Millisecond)
	assert.True(t, limiter.Allow(client), "client should be allowed after token refill")
}

func TestSearchPhotonLanguageParam(t *testing.T) {
	var capturedLang string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedLang = r.URL.Query().Get("lang")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"FeatureCollection","features":[]}`))
	}))
	defer server.Close()

	viper.Set("plugins.maps.photon_url", server.URL)
	defer viper.Set("plugins.maps.photon_url", "")

	plugin := &MapsPlugin{
		httpClient:    server.Client(),
		cache:         newGeocodeCache(time.Minute, time.Minute, 100),
		photonLimiter: newRateLimiter(time.Millisecond),
	}
	defer plugin.cache.Close()

	_, err := plugin.searchPhoton("tokyo", "en")
	require.NoError(t, err)
	assert.Equal(t, "en", capturedLang)

	_, err = plugin.searchPhoton("tokyo", "de")
	require.NoError(t, err)
	assert.Equal(t, "de", capturedLang)

	_, err = plugin.searchPhoton("tokyo", "")
	require.NoError(t, err)
	assert.Empty(t, capturedLang)
}

func TestSearchNominatimLanguageHeader(t *testing.T) {
	var capturedAcceptLang string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAcceptLang = r.Header.Get("Accept-Language")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	viper.Set("plugins.maps.nominatim_url", server.URL)
	defer viper.Set("plugins.maps.nominatim_url", "")

	plugin := &MapsPlugin{
		httpClient:       server.Client(),
		cache:            newGeocodeCache(time.Minute, time.Minute, 100),
		nominatimLimiter: newRateLimiter(time.Millisecond),
	}
	defer plugin.cache.Close()

	_, err := plugin.searchNominatim("london", "fr")
	require.NoError(t, err)
	assert.Equal(t, "fr", capturedAcceptLang)

	_, err = plugin.searchNominatim("london", "")
	require.NoError(t, err)
	assert.Equal(t, "en", capturedAcceptLang, "default fallback should be en")
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
			},
			{
				"type": "Feature",
				"geometry": {
					"coordinates": [999.0, -999.0],
					"type": "Point"
				},
				"properties": {
					"osm_id": 99999,
					"osm_type": "N",
					"name": "Invalid Out-of-Bounds Place"
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
		cache:            newGeocodeCache(time.Minute, time.Minute, 100),
		nominatimLimiter: newRateLimiter(time.Millisecond),
		photonLimiter:    newRateLimiter(time.Millisecond),
	}
	defer plugin.cache.Close()

	results, err := plugin.searchPhoton("eiffel", "")
	require.NoError(t, err)
	// Invalid out-of-bounds coordinates must be filtered out
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
		},
		{
			"place_id": 98766,
			"name": "Malformed Coords",
			"display_name": "Malformed Lat Lon",
			"lat": "not-a-number",
			"lon": "also-not-a-number"
		},
		{
			"place_id": 98767,
			"name": "Out Of Bounds Coords",
			"display_name": "Out Of Bounds",
			"lat": "999.0",
			"lon": "-999.0"
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
		cache:            newGeocodeCache(time.Minute, time.Minute, 100),
		nominatimLimiter: newRateLimiter(time.Millisecond),
	}
	defer plugin.cache.Close()

	results, err := plugin.searchNominatim("big ben", "")
	require.NoError(t, err)
	// Malformed and out-of-bounds coords must be rejected, not emitted as 0,0
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
		cache:            newGeocodeCache(time.Minute, time.Minute, 100),
		nominatimLimiter: newRateLimiter(time.Millisecond),
		photonLimiter:    newRateLimiter(time.Millisecond),
	}
	defer plugin.cache.Close()

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

func TestSearchGooglePlaces(t *testing.T) {
	fakeGoogleResponse := `{
		"places": [
			{
				"id": "ChIJ123",
				"displayName": {"text": "Googleplex"},
				"formattedAddress": "1600 Amphitheatre Pkwy, Mountain View, CA",
				"location": {"latitude": 37.422, "longitude": -122.084},
				"googleMapsUri": "https://maps.google.com/?cid=123"
			},
			{
				"id": "ChIJInvalid",
				"displayName": {"text": "Invalid Coordinates Place"},
				"formattedAddress": "Somewhere Invalid",
				"location": {"latitude": 100.0, "longitude": -200.0},
				"googleMapsUri": "https://maps.google.com/?cid=inv"
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "AIzaSyTestKey", r.Header.Get("X-Goog-Api-Key"))
		assert.Equal(t, "places.id,places.displayName,places.formattedAddress,places.location,places.googleMapsUri", r.Header.Get("X-Goog-FieldMask"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeGoogleResponse))
	}))
	defer server.Close()

	viper.Set("plugins.maps.google_api_key", "AIzaSyTestKey")
	viper.Set("plugins.maps.google_places_url", server.URL)
	defer func() {
		viper.Set("plugins.maps.google_api_key", "")
		viper.Set("plugins.maps.google_places_url", "")
	}()

	plugin := &MapsPlugin{
		httpClient:    server.Client(),
		cache:         newGeocodeCache(time.Minute, time.Minute, 100),
		googleLimiter: newRateLimiter(time.Millisecond),
	}
	defer plugin.cache.Close()

	results, err := plugin.searchGooglePlaces("googleplex")
	require.NoError(t, err)
	require.Len(t, results, 1, "invalid coordinates place must be filtered out")
	assert.Equal(t, "ChIJ123", results[0].PlaceID)
	assert.Equal(t, "Googleplex", results[0].DisplayName)
	assert.Equal(t, "1600 Amphitheatre Pkwy, Mountain View, CA", results[0].Address)
	assert.InDelta(t, 37.422, results[0].Latitude, 0.0001)
	assert.InDelta(t, -122.084, results[0].Longitude, 0.0001)
	assert.Equal(t, "google", results[0].Provider)
	assert.Equal(t, "https://maps.google.com/?cid=123", results[0].MapURL)

	// Here test searchGooglePlaces behavior with unconfigured key
	viper.Set("plugins.maps.google_api_key", "")
	_, err = plugin.searchGooglePlaces("googleplex")
	assert.Error(t, err, "should return error when API key is missing")
}

func TestQueryLengthBounding(t *testing.T) {
	fakePhotonEmpty := `{"type":"FeatureCollection","features":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		assert.LessOrEqual(t, len(q), 200, "query parameter sent upstream must be capped at 200 characters")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakePhotonEmpty))
	}))
	defer server.Close()

	viper.Set("plugins.maps.geocoder_provider", "photon")
	viper.Set("plugins.maps.photon_url", server.URL)
	defer func() {
		viper.Set("plugins.maps.geocoder_provider", "")
		viper.Set("plugins.maps.photon_url", "")
	}()

	plugin := &MapsPlugin{
		httpClient:    server.Client(),
		cache:         newGeocodeCache(time.Minute, time.Minute, 100),
		photonLimiter: newRateLimiter(time.Millisecond),
	}
	defer plugin.cache.Close()

	e := echo.New()
	veryLongQuery := strings.Repeat("a", 500)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/geocode?q="+veryLongQuery, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := plugin.handleGeocodeSearch(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUnauthorizedAccess(t *testing.T) {
	plugin := &MapsPlugin{}
	e := echo.New()
	plugin.RegisterAuthenticatedRoutes(e.Group("/api/v1/plugins"))

	// Request without user context
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/tasks/1/location", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/plugins/maps/tasks/1/location", strings.NewReader(`{"latitude":10,"longitude":20,"display_name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/maps/tasks/1/location", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/projects/1/locations", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestReadOnlyAccessForbidden(t *testing.T) {
	s := db.NewSession()
	_ = s.Table("users").Sync2(&user.User{})
	_ = s.Table("projects").Sync2(&models.Project{})
	_ = s.Table("tasks").Sync2(&models.Task{})
	_ = s.Table("task_locations").Sync2(&TaskLocation{})
	_ = s.Table("users_projects").Sync2(&models.ProjectUser{})

	owner := &user.User{ID: 101, Username: "projowner2"}
	readOnlyUser := &user.User{ID: 102, Username: "readonlyuser"}
	_, _ = s.Table("users").Insert(owner)
	_, _ = s.Table("users").Insert(readOnlyUser)

	p := &models.Project{ID: 8888, Title: "RO Project", OwnerID: owner.ID}
	_, _ = s.Table("projects").Insert(p)

	task := &models.Task{ID: 88881, ProjectID: p.ID, Title: "RO Task"}
	_, _ = s.Table("tasks").Insert(task)

	loc := &TaskLocation{
		TaskID:      task.ID,
		ProjectID:   p.ID,
		DisplayName: "RO Task Location",
		Latitude:    12.34,
		Longitude:   56.78,
		Provider:    "photon",
	}
	_, _ = s.Table("task_locations").Insert(loc)

	// Give readOnlyUser read-only permission (PermissionRead)
	upRO := &models.ProjectUser{UserID: readOnlyUser.ID, ProjectID: p.ID, Permission: models.PermissionRead}
	_, _ = s.Table("users_projects").Insert(upRO)

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
		_, _ = sClean.Table("tasks").Where("id = ?", 88881).Delete(&models.Task{})
		_, _ = sClean.Table("task_locations").Where("task_id = ?", 88881).Delete(&TaskLocation{})
		_, _ = sClean.Table("users_projects").Where("project_id = ?", 8888).Delete(&models.ProjectUser{})
		_, _ = sClean.Table("project_ancestors").Where("project_id = ?", 8888).Delete(map[string]interface{}{})
		_, _ = sClean.Table("projects").Where("id = ?", 8888).Delete(&models.Project{})
		_, _ = sClean.Table("users").Where("id IN (?, ?)", 101, 102).Delete(&user.User{})
		_ = sClean.Commit()
	}()

	plugin := &MapsPlugin{}
	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("api_user", readOnlyUser)
			return next(c)
		}
	})
	plugin.RegisterAuthenticatedRoutes(e.Group("/api/v1/plugins"))

	// 1. Read-only user CAN read task location
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/tasks/88881/location", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// 2. Read-only user CAN list project locations
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/projects/8888/locations", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// 3. Read-only user CANNOT update location (403 Forbidden)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/plugins/maps/tasks/88881/location", strings.NewReader(`{"latitude":15.0,"longitude":25.0,"display_name":"Hacked"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// 4. Read-only user CANNOT delete location (403 Forbidden)
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/maps/tasks/88881/location", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLocationCRUDLifecycle(t *testing.T) {
	s := db.NewSession()
	_ = s.Table("users").Sync2(&user.User{})
	_ = s.Table("projects").Sync2(&models.Project{})
	_ = s.Table("tasks").Sync2(&models.Task{})
	_ = s.Table("task_locations").Sync2(&TaskLocation{})
	_ = s.Table("users_projects").Sync2(&models.ProjectUser{})

	u := &user.User{ID: 201, Username: "cruduser"}
	_, _ = s.Table("users").Insert(u)

	p := &models.Project{ID: 9991, Title: "CRUD Project", OwnerID: u.ID}
	_, _ = s.Table("projects").Insert(p)

	task := &models.Task{ID: 99911, ProjectID: p.ID, Title: "CRUD Task"}
	_, _ = s.Table("tasks").Insert(task)

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
		_, _ = sClean.Table("tasks").Where("id = ?", 99911).Delete(&models.Task{})
		_, _ = sClean.Table("task_locations").Where("task_id = ?", 99911).Delete(&TaskLocation{})
		_, _ = sClean.Table("users_projects").Where("project_id = ?", 9991).Delete(&models.ProjectUser{})
		_, _ = sClean.Table("project_ancestors").Where("project_id = ?", 9991).Delete(map[string]interface{}{})
		_, _ = sClean.Table("projects").Where("id = ?", 9991).Delete(&models.Project{})
		_, _ = sClean.Table("users").Where("id = ?", 201).Delete(&user.User{})
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

	// Step 1: Create location (POST)
	body := `{"display_name":"Eiffel Tower","latitude":48.8584,"longitude":2.2945,"provider":"photon"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/maps/tasks/99911/location", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var created TaskLocation
	err := json.Unmarshal(rec.Body.Bytes(), &created)
	require.NoError(t, err)
	assert.Equal(t, "Eiffel Tower", created.DisplayName)
	assert.InDelta(t, 48.8584, created.Latitude, 0.0001)

	// Step 2: Read location (GET)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/tasks/99911/location", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var fetched TaskLocation
	err = json.Unmarshal(rec.Body.Bytes(), &fetched)
	require.NoError(t, err)
	assert.Equal(t, created.DisplayName, fetched.DisplayName)

	// Step 3: Update location (PUT)
	updateBody := `{"display_name":"Louvre Museum","latitude":48.8606,"longitude":2.3376,"provider":"photon"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/plugins/maps/tasks/99911/location", strings.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updated TaskLocation
	err = json.Unmarshal(rec.Body.Bytes(), &updated)
	require.NoError(t, err)
	assert.Equal(t, "Louvre Museum", updated.DisplayName)
	assert.InDelta(t, 48.8606, updated.Latitude, 0.0001)

	// Step 4: Delete location (DELETE)
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/maps/tasks/99911/location", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Step 5: Read again after delete (GET returns 200 with null body)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins/maps/tasks/99911/location", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "null\n", rec.Body.String())
}

func TestRateLimiterContextCancellation(t *testing.T) {
	limiter := newRateLimiter(5 * time.Second)
	// Primer request to set lastRequest
	err := limiter.Wait(context.Background())
	require.NoError(t, err)

	// Second request with cancelled context should abort immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	start := time.Now()
	err = limiter.Wait(ctx)
	duration := time.Since(start)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, duration, 200*time.Millisecond, "cancelled rate limiter wait should not block for full interval")
}

func TestCacheDoubleCheckRace(t *testing.T) {
	cache := newGeocodeCache(100*time.Millisecond, 50*time.Millisecond, 10)
	defer cache.Close()

	sampleA := []GeocodeSuggestion{{PlaceID: "A", DisplayName: "Location A"}}
	sampleB := []GeocodeSuggestion{{PlaceID: "B", DisplayName: "Location B"}}

	cache.Set("p", "key", sampleA)
	// Force entry to be expired
	cache.Lock()
	cache.entries["p:key"] = cacheEntry{
		suggestions: sampleA,
		expiresAt:   time.Now().Add(-1 * time.Second),
	}
	cache.Unlock()

	// Concurrently refresh with fresh entry
	cache.Set("p", "key", sampleB)

	// Get should find the fresh entry B, not delete it
	got, found := cache.Get("p", "key")
	assert.True(t, found)
	require.Len(t, got, 1)
	assert.Equal(t, "Location B", got[0].DisplayName)
}

func TestPluginMigrationLifecycle(t *testing.T) {
	x, err := db.CreateTestEngine()
	require.NoError(t, err)

	plugin := &MapsPlugin{}
	migrations := plugin.Migrations()
	require.Len(t, migrations, 1)

	m := migrations[0]
	assert.Equal(t, "20261018000001-create-task-locations-table", m.ID)

	// Test Rollback initially (drop table if exists)
	err = m.Rollback(x)
	require.NoError(t, err)

	// Test Migrate
	err = m.Migrate(x)
	require.NoError(t, err)

	// Verify table exists by inserting a record
	loc := &TaskLocation{
		TaskID:      12345,
		ProjectID:   67890,
		DisplayName: "Migration Test Place",
		Latitude:    12.34,
		Longitude:   56.78,
		Provider:    "photon",
	}
	_, err = x.Table("task_locations").Insert(loc)
	require.NoError(t, err)

	// Verify row is present
	var count int64
	count, err = x.Table("task_locations").Where("task_id = ?", 12345).Count()
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	// Test Rollback drops table
	err = m.Rollback(x)
	require.NoError(t, err)

	// Verify table no longer exists
	has, err := x.IsTableExist("task_locations")
	require.NoError(t, err)
	assert.False(t, has, "task_locations table should have been dropped by rollback")
}



