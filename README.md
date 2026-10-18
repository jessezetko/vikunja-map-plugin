# Vikunja Maps & Location Plugin

A modular **Maps & Location** feature for [Vikunja](https://vikunja.io/), adding work item location search, geocoding autocomplete, and an interactive **OpenStreetMap** project view tab.

Designed for vacation planning, travel itineraries, field service, and geo-tagged task management.

---

## Features

- 📍 **Work Item Location Field**: Autocomplete search directly within task details. Select an address or venue to automatically attach coordinates, display names, and external map links.
- 🗺️ **Interactive Project Map View**: A 5th view tab alongside List, Gantt, Table, and Kanban. Displays an interactive Leaflet/OpenStreetMap canvas with pins for all geocoded project tasks.
- 🔍 **Multiple Geocoding Providers**:
  - **OpenStreetMap Nominatim** *(Default, free, zero API key required)* with OSM usage policy compliance.
  - **Google Places API (New)** *(Optional)* with server-side API key protection.
- 🌓 **Adaptive Dark / Light Themes**: Automatically switches between CARTO Voyager (light) and Dark Matter (dark) tile layers to match your Vikunja theme.
- 💾 **Native Database Persistence**: Uses Vikunja's Yaegi migration plugin system to manage the `task_locations` table across SQLite, PostgreSQL, and MySQL.
- 🧹 **Automatic Cleanup**: Listens to Vikunja's `TaskDeletedEvent` via Watermill to prune location records when tasks are deleted.

---

## Repository Structure

```
vikunja-map-plugin/
├── LICENSE                        # GNU AGPL-3.0 License
├── README.md                      # Documentation & configuration guide
├── install.sh                     # Automated 1-step installer script
├── vikunja-core-maps.patch        # Clean patch for Vikunja core enum & frontend
├── plugins/
│   └── maps/
│       └── main.go                # Backend Yaegi Go plugin
└── frontend/
    └── src/
        ├── client/queries/maps.ts # API client queries for map endpoints
        ├── components/
        │   ├── project/views/
        │   │   └── ProjectViewMap.vue # Leaflet Map View tab
        │   └── tasks/partials/
        │       └── LocationSearchField.vue # Autocomplete task input
```

---

## Quick Installation

Run `install.sh` pointing to your Vikunja repository root:

```bash
git clone https://github.com/jessezetko/vikunja-map-plugin.git
cd vikunja-map-plugin
./install.sh /path/to/your/vikunja
```

The script will:
1. Copy the backend plugin into `plugins/maps/main.go`.
2. Apply `vikunja-core-maps.patch` to register the `Map` view kind and frontend components.
3. Install the required `leaflet` and `@types/leaflet` dependencies in `frontend/`.

---

## Manual Installation

If you prefer to install manually:

### 1. Backend Plugin
Copy `plugins/maps/main.go` to your Vikunja repository:
```bash
cp -r plugins/maps /path/to/vikunja/plugins/
```

### 2. Core & Frontend Integration
Apply the minimal patch from your Vikunja root:
```bash
cd /path/to/vikunja
git apply /path/to/vikunja-map-plugin/vikunja-core-maps.patch
```

### 3. Frontend Dependencies
Install Leaflet in the `frontend` directory:
```bash
cd /path/to/vikunja/frontend
pnpm add leaflet@^1.9.4
pnpm add -D @types/leaflet@^1.9.14
```

### 4. Build Vikunja
Build both frontend assets and the backend binary:
```bash
cd /path/to/vikunja/frontend
pnpm run build

cd /path/to/vikunja
mage build:build
```

---

## Configuration

Add the following to your Vikunja `config.yml`:

```yaml
plugins:
  loader: 'yaegi'
  maps:
    # Geocoding provider: "nominatim" (default) or "google"
    geocoder_provider: 'nominatim'
    
    # Optional: Custom Nominatim base URL (defaults to https://nominatim.openstreetmap.org)
    # nominatim_url: 'https://nominatim.openstreetmap.org'
    
    # Optional: Custom User-Agent header for Nominatim queries
    # nominatim_user_agent: 'Vikunja-Maps-Plugin/1.0 (contact@yourdomain.com)'
    
    # Optional: Google Places API Key (if geocoder_provider is set to "google")
    # google_api_key: 'AIzaSy...'
```

### Optional CARTO Basemap Key

For high-volume production deployments utilizing CARTO basemaps, set the environment variable:
```bash
export CARTO_BASEMAPS_API_KEY="your-carto-key"
```

---

## Architectural Notes

Vikunja's native plugin architecture currently executes backend Go plugins via the [Yaegi](https://github.com/traefik/yaegi) interpreter (`plugins.Plugin`, `plugins.MigrationPlugin`, and `plugins.AuthenticatedRouterPlugin`). 

Because Vikunja does not yet have a frontend plugin loader system, adding UI views (such as the Map view tab and Task Location input) requires building the frontend with the integrated components. `vikunja-core-maps.patch` makes the minimal changes necessary to bridge the backend plugin endpoints with the Vue 3 user interface.

---

## License & Attribution

This project is licensed under the **GNU Affero General Public License v3.0 (AGPL-3.0)**, matching Vikunja's licensing. See [LICENSE](LICENSE) for details.

### Third-Party Attribution
- **Map Data**: © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors.
- **Basemap Tiles**: © [CARTO](https://carto.com/attributions).
- **Map Rendering**: [Leaflet](https://leafletjs.com/) (BSD 2-Clause).
