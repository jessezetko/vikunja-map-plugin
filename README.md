# Vikunja Maps & Location Plugin

A complete **Maps & Location** feature for [Vikunja](https://vikunja.io/), adding work item location search, geocoding autocomplete, and an interactive **OpenStreetMap** project view tab.

Designed for vacation planning, travel itineraries, field service, and geo-tagged task management.

![Vikunja Project Map View](docs/images/map-view.png)

---

## Features

- 📍 **Work Item Location Field**: Search directly within task details. Select an address or venue to attach coordinates, display names, and map links.
- 🗺️ **Interactive Project Map View**: A 5th view tab alongside List, Gantt, Table, and Kanban. Displays an interactive Leaflet map canvas with pins for all geocoded project tasks. Pins dynamically reflect each work item's custom color attribute, and marker visibility syncs when tasks move across projects.
- 🔍 **Policy-Compliant Geocoding & Autocomplete**:
  - **Photon by Komoot** *(Default, free, zero API key required)*: OpenStreetMap-based geocoding specifically engineered for search-as-you-type autocomplete. Defaults to `https://photon.komoot.io` or connects seamlessly to a self-hosted instance (`komoot/photon`).
  - **OpenStreetMap Nominatim** *(Free, zero API key required)*: Strict compliance with [OSMF Nominatim Usage Policy](https://operations.osmfoundation.org/policies/nominatim/). Autocomplete-as-you-type is automatically disabled; search queries require pressing Enter or clicking the Search button. Includes server-side 24-hour query caching and a 1 request/second rate limiter.
  - **Google Places API** *(Optional)*: Commercial high-precision autocomplete with server-side API key protection.
- 🌓 **Zero-Key Map Tiles (Dark & Light Modes)**:
  - **Default (No API Key Required)**: Standard raster OpenStreetMap tiles (`tile.openstreetmap.org`) with CSS dark-mode inversion when switching themes.
  - **Optional CARTO Basemaps**: If configured with a [CARTO API key](https://carto.com/basemaps/), switches to CARTO Voyager (light) and Dark Matter (dark) tiles.
- 💾 **Native Database Persistence & Dynamic Membership**: Uses Vikunja's Yaegi migration system for the `task_locations` table across SQLite, PostgreSQL, and MySQL. Automatically joins tasks to guarantee markers reflect current task project locations.
- 🧹 **Automatic Cleanup & Sync**: Listens to Vikunja's `TaskUpdatedEvent` to keep location project associations in sync, `TaskDeletedEvent` to remove pins, and `ProjectDeletedEvent` to clean up orphaned project locations.

---

## How It Works (And Why a Build Is Required)

Vikunja plugins currently only run Go scripts on the backend via the [Yaegi](https://github.com/traefik/yaegi) interpreter. Because Vikunja does not yet have a frontend plugin system, the official Docker image (`vikunja/vikunja:latest`) cannot dynamically load new UI components.

To provide both the **Task Location field** and the **Map view tab**, this repository includes:
1. **The Backend Plugin** (`plugins/maps/main.go`): Exposes geocoding proxies, CRUD endpoints, and DB migrations.
2. **A Core & Frontend Integration Patch** (`vikunja-core-maps.patch`): Adds the `map` view enum and mounts the Vue 3 Leaflet components.

---

## Installation Method 1: Docker (Recommended for Self-Hosters)

**Pre-built Docker images are automatically published on every release to the GitHub Container Registry (`ghcr.io/jessezetko/vikunja-map-plugin:latest`).** No Go or Node.js required on your host machine.

### 1. Download `docker-compose.yml`
```bash
curl -O https://raw.githubusercontent.com/jessezetko/vikunja-map-plugin/main/docker-compose.example.yml
mv docker-compose.example.yml docker-compose.yml
```

### 2. Start Vikunja
```bash
docker compose up -d
```

Your custom Vikunja instance with the Maps plugin will be downloaded and accessible at `http://localhost:3456`.

*(Optional: To build the container image yourself from source instead of pulling from GHCR, clone this repository and run `docker compose up -d --build`)*

### Updating Your Instance

A nightly automated workflow checks for new upstream Vikunja releases, verifies patch compatibility, and publishes updated images to GHCR.

To update your running instance to the latest supported version:
```bash
docker compose pull vikunja
docker compose up -d
```

---

## Installation Method 2: Build From Source (For Developers)

If you compile Vikunja locally on bare metal:

### 1. Clone both repositories
```bash
git clone https://github.com/go-vikunja/vikunja.git
git clone https://github.com/jessezetko/vikunja-map-plugin.git
```

### 2. Run the automated installer
```bash
cd vikunja-map-plugin
./install.sh ../vikunja
```

### 3. Build & Run
```bash
cd ../vikunja/frontend
pnpm run build

cd ..
mage build:build
./vikunja
```

---

## Configuration Reference

### Understanding Map Tiles vs. Geocoding Providers

It is important to distinguish between **Map Tiles** and **Geocoding Providers**:
- **Map Tiles (Display)**: The raster image tiles displayed on the interactive map canvas. OpenStreetMap tiles (`https://tile.openstreetmap.org/`) are used by default with **zero API key required**. If `CARTO_BASEMAPS_API_KEY` is provided, CARTO Voyager and Dark Matter tiles are used instead.
- **Geocoding Providers (Search & Autocomplete)**: The service that transforms search strings (e.g. "Eiffel Tower") into geographic coordinates (`latitude`, `longitude`).
  - **Photon** (`photon`): **Default**. Designed specifically for search-as-you-type. Free public instance hosted by Komoot (`https://photon.komoot.io`), or self-host your own via Docker (`docker run -d -p 2322:2322 komoot/photon`).
    > **Note on International Locations & Localization**: Photon indexes OpenStreetMap data using the native language and local script of each region (for example, queries in Japan, Greece, or the Middle East will return results rendered in Japanese kanji/kana, Greek, or Arabic rather than transliterated English). If your workflow requires consistent English-transliterated results for non-Latin international regions or commercial-grade global address resolution, use the **Google Places** provider (`google`) with a `google_api_key`.
  - **Nominatim** (`nominatim`): Standard OSM Nominatim. Keystroke autocomplete is automatically disabled in the UI; users must press Enter or click "Search" to trigger queries, complying with OSMF usage policy.
  - **Google Places** (`google`): High-precision Google Places New API. Requires `google_api_key`. Ideal for consistent multi-language and English-localized global search.

### Docker Compose Environment Variables
```yaml
environment:
  VIKUNJA_PLUGINS_ENABLED: "true"
  VIKUNJA_PLUGINS_LOADER: "yaegi"
  VIKUNJA_PLUGINS_DIR: "/app/vikunja/plugins"
  # Geocoder Provider: "photon" (default), "nominatim", or "google"
  VIKUNJA_PLUGINS_MAPS_GEOCODER_PROVIDER: "photon"
  # VIKUNJA_PLUGINS_MAPS_PHOTON_URL: "https://photon.komoot.io" # or http://photon:2322
  # VIKUNJA_PLUGINS_MAPS_NOMINATIM_URL: "https://nominatim.openstreetmap.org"
  # VIKUNJA_PLUGINS_MAPS_GOOGLE_API_KEY: "AIzaSy..."
  # Optional CARTO basemaps API key (required ONLY if using CARTO tiles instead of OpenStreetMap)
  # CARTO_BASEMAPS_API_KEY: "your-carto-key"
```

### Or in `config.yml`
```yaml
plugins:
  enabled: true
  loader: 'yaegi'
  dir: '/app/vikunja/plugins'
  maps:
    geocoder_provider: 'photon' # 'photon' (default), 'nominatim', or 'google'
    # photon_url: 'https://photon.komoot.io' # or custom self-hosted URL
    # nominatim_url: 'https://nominatim.openstreetmap.org'
    # google_api_key: 'AIzaSy...'
```

---

## How to Use in the Web Interface

### 1. Adding a Location to a Task
1. Open any task in Vikunja to view the task detail modal.
2. In the right-hand sidebar under **Attributes**, click **Set Location** (pin icon).
3. Type an address, landmark, or city into the search input. Suggestions appear automatically via Nominatim or Google Places:

![Location Autocomplete Search](docs/images/location-autocomplete.png)

4. Select the location from the dropdown suggestions. The structured coordinates and address are saved immediately.
5. Once saved, a compact location badge appears with an external link to view the coordinates directly on OpenStreetMap:

![Saved Work Item Location](docs/images/task-location-saved.png)

### 2. Viewing Projects on the Map
1. Navigate to any project.
2. In the project view bar (next to *List*, *Gantt*, *Table*, *Kanban*), click the **`+`** button to add a view.
3. Select **Map** and click **Save**.
4. The interactive map renders pins for all geocoded tasks in the project (respecting each work item's custom color), complete with pin popups, a list sidebar, and a **Fit all pins** control:

![Vikunja Project Map View](docs/images/map-view.png)

---

## Verification

To verify that the plugin is running properly:
```bash
docker compose logs vikunja | grep -i "maps"
```
You should see:
```text
INFO Loaded plugin maps
INFO Vikunja Maps plugin initialized (provider=nominatim)
```

---

## License & Attribution

This project is licensed under the **GNU Affero General Public License v3.0 (AGPL-3.0)**, matching Vikunja's licensing. See [LICENSE](LICENSE) for details.

### Third-Party Attribution
- **Map Data**: © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors.
- **Basemap Tiles**: © [CARTO](https://carto.com/attributions).
- **Map Rendering**: [Leaflet](https://leafletjs.com/) (BSD 2-Clause).
