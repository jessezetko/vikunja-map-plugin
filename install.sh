#!/usr/bin/env bash
set -euo pipefail

# Vikunja Maps Plugin Installer
# Usage: ./install.sh [path/to/vikunja]

TARGET_DIR="${1:-.}"

if [ ! -f "${TARGET_DIR}/pkg/models/project_view.go" ] || [ ! -d "${TARGET_DIR}/frontend" ]; then
    echo "Error: Target directory '${TARGET_DIR}' does not appear to be a Vikunja repository root."
    echo "Please specify the path to your Vikunja root, e.g.: ./install.sh /path/to/vikunja"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Installing Vikunja Maps Plugin into: ${TARGET_DIR}"

# 1. Ensure plugins/maps directory exists and copy the plugin main.go
echo "--> Installing backend plugin..."
mkdir -p "${TARGET_DIR}/plugins/maps"
cp "${SCRIPT_DIR}/plugins/maps/main.go" "${TARGET_DIR}/plugins/maps/main.go"

# 2. Apply core and frontend patch
echo "--> Applying core and frontend integration patch..."
if (cd "${TARGET_DIR}" && git apply --check "${SCRIPT_DIR}/vikunja-core-maps.patch" 2>/dev/null); then
    (cd "${TARGET_DIR}" && git apply "${SCRIPT_DIR}/vikunja-core-maps.patch")
    echo "    Patch applied cleanly via git apply."
else
    echo "    Warning: git apply --check had conflicts or was already applied."
    echo "    Attempting 3-way merge fallback or file copy..."
    (cd "${TARGET_DIR}" && git apply -3 "${SCRIPT_DIR}/vikunja-core-maps.patch" 2>/dev/null) || {
        echo "    Copying individual frontend components directly..."
        mkdir -p "${TARGET_DIR}/frontend/src/client/queries"
        mkdir -p "${TARGET_DIR}/frontend/src/components/project/views"
        mkdir -p "${TARGET_DIR}/frontend/src/components/tasks/partials"
        cp "${SCRIPT_DIR}/frontend/src/client/queries/maps.ts" "${TARGET_DIR}/frontend/src/client/queries/"
        cp "${SCRIPT_DIR}/frontend/src/components/project/views/ProjectViewMap.vue" "${TARGET_DIR}/frontend/src/components/project/views/"
        cp "${SCRIPT_DIR}/frontend/src/components/tasks/partials/LocationSearchField.vue" "${TARGET_DIR}/frontend/src/components/tasks/partials/"
    }
fi

# 3. Ensure frontend Leaflet dependencies are installed
if [ -d "${TARGET_DIR}/frontend" ]; then
    echo "--> Installing Leaflet frontend dependencies..."
    (cd "${TARGET_DIR}/frontend" && command -v pnpm >/dev/null 2>&1 && pnpm add leaflet@^1.9.4 && pnpm add -D @types/leaflet@^1.9.14) || {
        echo "    Note: Run 'pnpm add leaflet @types/leaflet' inside ${TARGET_DIR}/frontend"
    }
fi

echo ""
echo "=========================================================="
echo " Vikunja Maps Plugin installed successfully!"
echo "=========================================================="
echo ""
echo "Next steps:"
echo "1. Enable plugins in your Vikunja config.yml:"
echo "   plugins:"
echo "     loader: 'yaegi'"
echo "     maps:"
echo "       geocoder_provider: 'nominatim' # or 'google'"
echo "       # google_api_key: 'AIzaSy...'"
echo ""
echo "2. Rebuild Vikunja frontend & backend:"
echo "   cd frontend && pnpm run build"
echo "   mage build:build"
echo ""
