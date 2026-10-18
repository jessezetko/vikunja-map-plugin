<template>
	<ProjectWrapper
		class="project-map-view-wrapper"
		:is-loading-project="isLoadingProject"
		:project-id="projectId"
		:view-id="viewId"
	>
		<template
			v-if="!isLoadingLocations && mappedTasks.length > 0"
			#header
		>
			<div class="map-toolbar">
				<BaseButton
					size="small"
					variant="secondary"
					@click="fitAllMarkers"
				>
					<Icon
						icon="map-marker-alt"
						class="mie-1"
					/>
					{{ $t('project.map.fitAll') }}
				</BaseButton>

				<BaseButton
					size="small"
					variant="secondary"
					class="mobile-locations-btn mis-2"
					@click="isMobileDrawerOpen = true"
				>
					<Icon
						icon="list"
						class="mie-1"
					/>
					{{ $t('task.location.title') }} ({{ mappedTasks.length }})
				</BaseButton>
			</div>
		</template>

		<div class="project-map-view">
			<div class="map-layout">
				<div
					ref="mapContainer"
					class="leaflet-map-canvas"
					:class="{'dark-osm-tiles': isUsingOsmFallback && isDark}"
				/>

				<!-- Desktop Persistent Sidebar -->
				<aside
					v-if="!isLoadingLocations && mappedTasks.length > 0"
					class="map-sidebar desktop-only"
				>
					<h4 class="sidebar-heading">
						<Icon
							icon="map-marker-alt"
							class="mie-1"
						/>
						{{ $t('task.location.title') }} ({{ mappedTasks.length }})
					</h4>
					<ul class="pinned-locations-list">
						<li
							v-for="item in mappedTasks"
							:key="item.task.id"
							class="pinned-location-item"
							@click="focusTaskPin(item.task.id)"
						>
							<div class="pinned-task-title">
								{{ item.task.title }}
							</div>
							<div class="pinned-task-address">
								{{ item.location.address || item.location.title }}
							</div>
						</li>
					</ul>
				</aside>

				<!-- Mobile Slide-Up Drawer -->
				<div
					v-if="!isLoadingLocations && mappedTasks.length > 0 && isMobileDrawerOpen"
					class="mobile-drawer-backdrop"
					@click="isMobileDrawerOpen = false"
				>
					<div
						class="mobile-drawer-panel"
						@click.stop
					>
						<div class="mobile-drawer-header">
							<span class="mobile-drawer-title">
								<Icon
									icon="map-marker-alt"
									class="mie-1"
								/>
								{{ $t('task.location.title') }} ({{ mappedTasks.length }})
							</span>
							<button
								type="button"
								class="mobile-drawer-close"
								:aria-label="$t('misc.closeDialog')"
								@click="isMobileDrawerOpen = false"
							>
								<Icon icon="xmark" />
							</button>
						</div>
						<ul class="pinned-locations-list">
							<li
								v-for="item in mappedTasks"
								:key="item.task.id"
								class="pinned-location-item"
								@click="onMobileLocationClick(item.task.id)"
							>
								<div class="pinned-task-title">
									{{ item.task.title }}
								</div>
								<div class="pinned-task-address">
									{{ item.location.address || item.location.title }}
								</div>
							</li>
						</ul>
					</div>
				</div>

				<!-- Empty hint shown ONLY after loading finishes and no locations exist -->
				<div
					v-else-if="!isLoadingLocations && mappedTasks.length === 0"
					class="empty-map-container"
				>
					<div class="empty-map-hint">
						<Icon
							icon="map"
							class="empty-map-icon mie-3"
						/>
						<span class="empty-map-text">{{ $t('project.map.noLocations') }}</span>
					</div>
				</div>
			</div>
		</div>
	</ProjectWrapper>
</template>

<script setup lang="ts">
import {computed, nextTick, onBeforeUnmount, onMounted, ref, watch} from 'vue'
import {useResizeObserver} from '@vueuse/core'
import {useRouter} from 'vue-router'
import L from 'leaflet'
import 'leaflet/dist/leaflet.css'

import BaseButton from '@/components/base/BaseButton.vue'
import Icon from '@/components/misc/Icon'
import ProjectWrapper from '@/components/project/ProjectWrapper.vue'
import {getProjectLocations, type TaskLocation} from '@/client/queries/maps'
import {useTaskList} from '@/composables/useTaskList'
import {useColorScheme} from '@/composables/useColorScheme'

const props = withDefaults(defineProps<{
	projectId: number
	viewId: number
	isLoadingProject?: boolean
}>(), {
	isLoadingProject: false,
})

const router = useRouter()
const {isDark} = useColorScheme()
const isMobileDrawerOpen = ref(false)

const mapContainer = ref<HTMLElement | null>(null)
const locationsByTaskId = ref<Map<number, TaskLocation>>(new Map())
const isLoadingLocations = ref(true)

let mapInstance: L.Map | null = null
let currentTileLayer: L.TileLayer | null = null
let markersLayer: L.LayerGroup | null = null
const markerByTaskId = new Map<number, L.Marker>()

const {tasks} = useTaskList(
	() => props.projectId,
	() => props.viewId,
)

const mappedTasks = computed(() => {
	const list: Array<{task: (typeof tasks.value)[0]; location: TaskLocation}> = []
	for (const t of tasks.value) {
		if (t.id && locationsByTaskId.value.has(t.id)) {
			list.push({task: t, location: locationsByTaskId.value.get(t.id)!})
		}
	}
	return list
})

const cartoApiKey = computed(() => {
	return (window as unknown as { CARTO_API_KEY?: string }).CARTO_API_KEY || ''
})

const isUsingOsmFallback = computed(() => {
	return !cartoApiKey.value
})

const tileLayerUrl = computed(() => {
	if (cartoApiKey.value) {
		const query = `?key=${encodeURIComponent(cartoApiKey.value)}`
		return isDark.value
			? `https://basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png${query}`
			: `https://basemaps.cartocdn.com/rastertiles/voyager/{z}/{x}/{y}.png${query}`
	}
	return 'https://tile.openstreetmap.org/{z}/{x}/{y}.png'
})

const tileAttribution = computed(() => {
	if (cartoApiKey.value) {
		return '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors &copy; <a href="https://carto.com/attributions">CARTO</a>'
	}
	return '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
})

const customMarkerIcon = computed(() => {
	const primaryColor = isDark.value ? '#3a86ff' : '#1976d2'
	return L.divIcon({
		className: 'custom-map-pin',
		html: `<div style="
			background-color: ${primaryColor};
			width: 30px;
			height: 30px;
			border-radius: 50% 50% 50% 0;
			transform: rotate(-45deg);
			display: flex;
			align-items: center;
			justify-content: center;
			box-shadow: 0 4px 10px rgba(0,0,0,0.35);
			border: 2px solid #ffffff;
		"><div style="
			width: 10px;
			height: 10px;
			background-color: #ffffff;
			border-radius: 50%;
			transform: rotate(45deg);
		"></div></div>`,
		iconSize: [30, 30],
		iconAnchor: [15, 30],
		popupAnchor: [0, -30],
	})
})

async function fetchLocations() {
	if (!props.projectId) return
	try {
		isLoadingLocations.value = true
		const list = await getProjectLocations(props.projectId)
		const map = new Map<number, TaskLocation>()
		for (const loc of list) {
			map.set(loc.task_id, loc)
		}
		locationsByTaskId.value = map
		renderMarkers()
	} catch {
		// Ignore error
	} finally {
		isLoadingLocations.value = false
	}
}

function initMap() {
	if (!mapContainer.value || mapInstance) return
	mapInstance = L.map(mapContainer.value, {
		zoomControl: true,
	}).setView([20, 0], 2)

	currentTileLayer = L.tileLayer(tileLayerUrl.value, {
		maxZoom: 19,
		attribution: tileAttribution.value,
	}).addTo(mapInstance)

	markersLayer = L.layerGroup().addTo(mapInstance)
}

function renderMarkers() {
	if (!mapInstance || !markersLayer) return
	markersLayer.clearLayers()
	markerByTaskId.clear()

	const latLngs: L.LatLngExpression[] = []

	for (const {task, location} of mappedTasks.value) {
		const coords: L.LatLngExpression = [location.latitude, location.longitude]
		latLngs.push(coords)

		const popupEl = document.createElement('div')
		popupEl.className = 'map-marker-popup'

		const titleEl = document.createElement('div')
		titleEl.className = 'marker-popup-title'
		titleEl.textContent = task.title
		popupEl.appendChild(titleEl)

		if (location.address || location.title) {
			const placeEl = document.createElement('div')
			placeEl.className = 'marker-popup-address'
			placeEl.textContent = location.address || location.title || ''
			popupEl.appendChild(placeEl)
		}

		const openBtn = document.createElement('button')
		openBtn.type = 'button'
		openBtn.className = 'button is-small is-primary is-fullwidth'
		openBtn.style.marginTop = '10px'
		openBtn.textContent = 'Open Work Item'
		openBtn.addEventListener('click', () => {
			router.push({
				name: 'task.detail',
				params: {id: task.id},
			})
		})
		popupEl.appendChild(openBtn)

		const marker = L.marker(coords, {icon: customMarkerIcon.value}).bindPopup(popupEl)
		marker.addTo(markersLayer)
		if (task.id) {
			markerByTaskId.set(task.id, marker)
		}
	}

	if (latLngs.length > 0) {
		mapInstance.fitBounds(L.latLngBounds(latLngs), {padding: [50, 50], maxZoom: 15})
	}
}

function fitAllMarkers() {
	if (!mapInstance || mappedTasks.value.length === 0) return
	const coords = mappedTasks.value.map(m => [m.location.latitude, m.location.longitude] as L.LatLngTuple)
	mapInstance.fitBounds(L.latLngBounds(coords), {padding: [50, 50], maxZoom: 15})
}

function focusTaskPin(taskId: number) {
	const marker = markerByTaskId.get(taskId)
	if (!marker || !mapInstance) return
	mapInstance.setView(marker.getLatLng(), 16, {animate: true})
	marker.openPopup()
}

function onMobileLocationClick(taskId?: number) {
	if (!taskId) return
	focusTaskPin(taskId)
	isMobileDrawerOpen.value = false
}

function tryInitAndRender() {
	if (mapContainer.value && !mapInstance) {
		initMap()
	}
	if (mapInstance) {
		nextTick(() => {
			mapInstance?.invalidateSize()
			renderMarkers()
		})
	}
}

watch([tileLayerUrl, tileAttribution], ([newUrl]) => {
	if (currentTileLayer) {
		currentTileLayer.setUrl(newUrl)
	}
	renderMarkers()
})

watch(() => [props.projectId, tasks.value], () => {
	fetchLocations()
}, {deep: true})

watch(mappedTasks, () => {
	renderMarkers()
	if (mapInstance) {
		nextTick(() => {
			mapInstance?.invalidateSize()
		})
	}
})

watch(mapContainer, (el) => {
	if (el) {
		nextTick(() => {
			tryInitAndRender()
		})
	}
})

watch(() => props.isLoadingProject, (loading) => {
	if (!loading) {
		nextTick(() => {
			tryInitAndRender()
		})
	}
})

useResizeObserver(mapContainer, () => {
	mapInstance?.invalidateSize()
})

onMounted(async () => {
	await nextTick()
	tryInitAndRender()
	await fetchLocations()
	await nextTick()
	tryInitAndRender()
})

onBeforeUnmount(() => {
	if (mapInstance) {
		mapInstance.remove()
		mapInstance = null
		currentTileLayer = null
		markersLayer = null
	}
})
</script>

<style scoped lang="scss">
.project-map-view-wrapper {
	inline-size: 100%;
}

.map-toolbar {
	display: flex;
	align-items: center;
}

.mobile-locations-btn {
	@media screen and (min-width: $tablet) {
		display: none !important;
	}
}

.project-map-view {
	inline-size: 100%;
	block-size: calc(100vh - #{$navbar-height} - 1.5rem - #{$switch-view-height} - 1rem - 1.5rem);
	min-block-size: 500px;
	border-radius: $radius;
	overflow: hidden;
	border: 1px solid var(--border);
	background: var(--scheme-main);
	position: relative;
	display: flex;

	@media screen and (max-width: $tablet) {
		block-size: calc(100vh - #{$navbar-height} - 1.5rem - (#{$switch-view-height} * 2) - 1rem - 1rem);
		min-block-size: 380px;
	}
}

.map-layout {
	display: flex;
	inline-size: 100%;
	block-size: 100%;
	flex: 1;
	min-block-size: 0;
	position: relative;
	overflow: hidden;
}

.leaflet-map-canvas {
	flex: 1;
	inline-size: 100%;
	block-size: 100%;
	min-block-size: 0;
	position: relative;
	z-index: 1;

	&.dark-osm-tiles :deep(.leaflet-tile-pane) {
		filter: brightness(0.6) invert(1) contrast(3) hue-rotate(200deg) saturate(0.3) brightness(0.7);
	}
}

.map-sidebar {
	inline-size: 320px;
	block-size: 100%;
	min-block-size: 0;
	background: var(--scheme-main);
	border-inline-start: 1px solid var(--border);
	overflow-y: auto;
	padding: 1.25rem 1rem;
	z-index: 10;
	display: flex;
	flex-direction: column;
	flex-shrink: 0;
	box-shadow: -4px 0 16px rgba(0, 0, 0, 0.08);

	:global(.dark) & {
		box-shadow: -4px 0 16px rgba(0, 0, 0, 0.35);
	}

	&.desktop-only {
		@media screen and (max-width: $tablet) {
			display: none !important;
		}
	}
}

.sidebar-heading {
	font-size: 0.95rem;
	font-weight: 600;
	color: var(--text-strong);
	margin-block-end: 1rem;
	display: flex;
	align-items: center;
}

.mobile-drawer-backdrop {
	position: absolute;
	inset: 0;
	z-index: 1000;
	background: rgba(0, 0, 0, 0.45);
	backdrop-filter: blur(2px);
	display: flex;
	flex-direction: column;
	justify-content: flex-end;

	@media screen and (min-width: $tablet) {
		display: none !important;
	}
}

.mobile-drawer-panel {
	background: var(--scheme-main);
	border-radius: $radius $radius 0 0;
	border-block-start: 1px solid var(--border);
	padding: 1rem;
	max-block-size: 55vh;
	overflow-y: auto;
	box-shadow: 0 -8px 24px rgba(0, 0, 0, 0.25);
	animation: slide-up 0.2s cubic-bezier(0.16, 1, 0.3, 1);
}

@keyframes slide-up {
	from {
		transform: translateY(100%);
	}
	to {
		transform: translateY(0);
	}
}

.mobile-drawer-header {
	display: flex;
	align-items: center;
	justify-content: space-between;
	margin-block-end: 0.85rem;
	padding-block-end: 0.5rem;
	border-block-end: 1px solid var(--border);
}

.mobile-drawer-title {
	font-weight: 700;
	font-size: 0.95rem;
	color: var(--text-strong);
}

.mobile-drawer-close {
	background: transparent;
	border: none;
	color: var(--text-light);
	font-size: 1rem;
	cursor: pointer;
	padding: 0.25rem;
	display: flex;
	align-items: center;
	justify-content: center;

	&:hover {
		color: var(--text);
	}
}

.pinned-locations-list {
	list-style: none;
	margin: 0;
	padding: 0;
	display: flex;
	flex-direction: column;
	gap: 0.6rem;
}

.pinned-location-item {
	padding: 0.75rem 0.85rem;
	border-radius: $radius;
	border: 1px solid var(--border);
	background: var(--grey-100);
	cursor: pointer;
	transition: background $transition, border-color $transition, transform 0.15s ease;

	&:hover {
		background: var(--grey-200);
		border-color: var(--primary);
		transform: translateY(-1px);
	}
}

.pinned-task-title {
	font-weight: 600;
	font-size: 0.875rem;
	color: var(--text-strong);
}

.pinned-task-address {
	font-size: 0.75rem;
	color: var(--text-light);
	margin-block-start: 0.25rem;
	white-space: nowrap;
	overflow: hidden;
	text-overflow: ellipsis;
}

.empty-map-container {
	position: absolute;
	inset-block-start: 2.5rem;
	inset-inline: 0;
	display: flex;
	justify-content: center;
	align-items: center;
	z-index: 1000;
	pointer-events: none;
}

.empty-map-hint {
	pointer-events: auto;
	display: inline-flex;
	align-items: center;
	background-color: hsl(204, 85%, 96%);
	border: 1.5px solid hsl(204, 75%, 60%);
	padding: 0.85rem 1.75rem;
	border-radius: var(--border-radius, 8px);
	box-shadow: 0 6px 20px rgba(0, 0, 0, 0.18);
	color: hsl(204, 85%, 22%);
	font-weight: 600;
	font-size: 0.95rem;

	:global(.dark) & {
		background-color: hsl(215, 30%, 18%);
		border-color: hsl(215, 70%, 55%);
		color: hsl(210, 20%, 98%);
		box-shadow: 0 6px 20px rgba(0, 0, 0, 0.45);
	}
}

.empty-map-icon {
	color: hsl(204, 85%, 45%);
	flex-shrink: 0;

	:global(.dark) & {
		color: hsl(215, 80%, 65%);
	}
}

:deep(.leaflet-popup-content-wrapper) {
	background: var(--scheme-main);
	color: var(--text);
	border-radius: $radius;
	box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
	border: 1px solid var(--border);
	padding: 0;
}

:deep(.leaflet-popup-tip) {
	background: var(--scheme-main);
}

:deep(.leaflet-popup-content) {
	margin: 0.85rem 1rem;
	line-height: 1.4;
	color: var(--text);

	.marker-popup-title {
		font-weight: 700;
		font-size: 0.95rem;
		color: var(--text-strong);
	}

	.marker-popup-address {
		font-size: 0.78rem;
		color: var(--text-light);
		margin-block-start: 0.25rem;
	}
}

:deep(.leaflet-container) {
	inline-size: 100%;
	block-size: 100%;
	font-family: inherit;
	background: var(--scheme-main);
}
</style>
