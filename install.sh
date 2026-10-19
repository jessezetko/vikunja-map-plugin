#!/usr/bin/env bash
set -euo pipefail

# Vikunja Maps Plugin Installer
# Usage: ./install.sh [path/to/vikunja]

TARGET_DIR="${1:-.}"
TARGET_DIR="$(cd "${TARGET_DIR}" && pwd)"

echo "==> Inspecting target directory: ${TARGET_DIR}"

# Pre-flight check 1: Target directory validation
if [ ! -f "${TARGET_DIR}/pkg/models/project_view.go" ] || [ ! -d "${TARGET_DIR}/frontend" ]; then
    echo "Error: Target directory '${TARGET_DIR}' does not appear to be a Vikunja repository root."
    echo "Please specify the path to your Vikunja root, e.g.: ./install.sh /path/to/vikunja"
    exit 1
fi

# Pre-flight check 2: Verify git repository
if ! (cd "${TARGET_DIR}" && git rev-parse --is-inside-work-tree >/dev/null 2>&1); then
    echo "Error: Target directory '${TARGET_DIR}' is not a Git repository."
    echo "The maps integration patch requires a Git-tracked Vikunja repository."
    exit 1
fi

# Pre-flight check 3: Target revision check
CURRENT_REF="$(cd "${TARGET_DIR}" && git describe --tags --always 2>/dev/null || echo "unknown")"
echo "    Target Vikunja revision: ${CURRENT_REF}"
if [[ "${CURRENT_REF}" != *"v2.7.0"* ]]; then
    echo "    Notice: The maps patch was tested against Vikunja v2.7.0. Current ref is '${CURRENT_REF}'."
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Installing Vikunja Maps Plugin into: ${TARGET_DIR}"

# 1. Apply core and frontend integration patch FIRST (prevents collision with existing files)
echo "--> Checking integration patch compatibility..."
if ! (cd "${TARGET_DIR}" && git apply --check "${SCRIPT_DIR}/vikunja-core-maps.patch"); then
    echo "Error: Patch cannot be applied cleanly to ${TARGET_DIR}."
    echo "Check if the patch has already been applied, or if you have conflicting uncommitted changes."
    exit 1
fi

echo "--> Applying core and frontend integration patch..."
(cd "${TARGET_DIR}" && git apply "${SCRIPT_DIR}/vikunja-core-maps.patch")
echo "    Integration patch applied cleanly."

# 2. Ensure plugins/maps directory exists and synchronize the plugin main.go
echo "--> Synchronizing backend plugin files..."
mkdir -p "${TARGET_DIR}/plugins/maps"
cp "${SCRIPT_DIR}/plugins/maps/main.go" "${TARGET_DIR}/plugins/maps/main.go"
if [ -f "${SCRIPT_DIR}/frontend/src/components/tasks/partials/LocationSearchField.spec.ts" ]; then
    mkdir -p "${TARGET_DIR}/frontend/src/components/tasks/partials"
    cp "${SCRIPT_DIR}/frontend/src/components/tasks/partials/LocationSearchField.spec.ts" "${TARGET_DIR}/frontend/src/components/tasks/partials/LocationSearchField.spec.ts"
fi

# 3. Ensure frontend Leaflet dependencies are installed
if [ -d "${TARGET_DIR}/frontend" ]; then
    echo "--> Installing frontend dependencies..."
    (cd "${TARGET_DIR}/frontend" && {
        if command -v pnpm >/dev/null 2>&1; then
            pnpm install || echo "    Warning: 'pnpm install' failed. Please run 'pnpm install' manually inside ${TARGET_DIR}/frontend"
        elif command -v corepack >/dev/null 2>&1; then
            corepack pnpm install || echo "    Warning: 'corepack pnpm install' failed. Please run 'pnpm install' manually inside ${TARGET_DIR}/frontend"
        else
            echo "    Note: Please run 'pnpm install' inside ${TARGET_DIR}/frontend"
        fi
    }) || true
fi

# 4. Post-installation validation checks
echo "--> Verifying installation integrity..."
MISSING_COMPONENTS=0

if ! grep -q "ProjectViewKindMap" "${TARGET_DIR}/pkg/models/project_view.go" 2>/dev/null; then
    echo "Error: Post-install check failed: ProjectViewKindMap missing from pkg/models/project_view.go"
    MISSING_COMPONENTS=1
fi

if ! grep -q "MAP: 'map'" "${TARGET_DIR}/frontend/src/constants/projectView.ts" 2>/dev/null; then
    echo "Error: Post-install check failed: PROJECT_VIEW_KINDS.MAP missing from frontend/src/constants/projectView.ts"
    MISSING_COMPONENTS=1
fi

if [ ! -s "${TARGET_DIR}/plugins/maps/main.go" ]; then
    echo "Error: Post-install check failed: plugins/maps/main.go is missing or empty"
    MISSING_COMPONENTS=1
fi

if [ ! -s "${TARGET_DIR}/frontend/src/components/project/views/ProjectViewMap.vue" ]; then
    echo "Error: Post-install check failed: ProjectViewMap.vue missing from frontend"
    MISSING_COMPONENTS=1
fi

if [ ! -s "${TARGET_DIR}/frontend/src/components/tasks/partials/LocationSearchField.vue" ]; then
    echo "Error: Post-install check failed: LocationSearchField.vue missing from frontend"
    MISSING_COMPONENTS=1
fi

if [ "${MISSING_COMPONENTS}" -ne 0 ]; then
    echo "Installation failed integrity checks."
    exit 1
fi

echo ""
echo "=========================================================="
echo " Vikunja Maps Plugin installed successfully!"
echo "=========================================================="
echo ""
echo "Next steps:"
echo "1. Enable plugins in your Vikunja config.yml:"
echo "   plugins:"
echo "     enabled: true"
echo "     loader: 'yaegi'"
echo "     maps:"
echo "       geocoder_provider: 'photon' # 'photon' (default), 'nominatim', or 'google'"
echo "       # photon_url: 'https://photon.komoot.io'"
echo "       # google_api_key: 'AIzaSy...'"
echo ""
echo "2. Rebuild Vikunja frontend & backend:"
echo "   cd ${TARGET_DIR}/frontend && pnpm run build"
echo "   cd ${TARGET_DIR} && mage build:build"
echo ""
