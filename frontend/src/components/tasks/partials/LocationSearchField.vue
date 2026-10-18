<template>
	<div class="task-location-field">
		<!-- Saved Location: Compact tag matching other Vikunja tags/labels -->
		<div
			v-if="savedLocation && !isEditing"
			class="location-tag-container"
		>
			<span
				class="tag location-tag"
				:class="{'is-clickable': !disabled}"
				:title="savedLocation.address || savedLocation.title"
				@click="startEditing"
			>
				<span class="location-tag-icon">
					<Icon icon="map-marker-alt" />
				</span>
				<span class="location-tag-text">
					{{ savedLocation.title || savedLocation.address }}
				</span>
				<a
					v-if="mapUrl"
					:href="mapUrl"
					target="_blank"
					rel="noopener noreferrer"
					class="location-map-link"
					:title="$t('task.location.viewOnMap')"
					@click.stop
				>
					<Icon icon="external-link-alt" />
				</a>
				<BaseButton
					v-if="!disabled"
					class="delete is-small"
					:aria-label="$t('task.location.remove')"
					@click.stop="clearLocation"
				/>
			</span>
		</div>

		<!-- Location Autocomplete Search Field -->
		<div
			v-else
			class="location-autocomplete"
		>
			<div
				class="control has-icons-left"
				:class="{'is-loading': isSearching}"
			>
				<input
					ref="inputRef"
					v-model="searchQuery"
					type="text"
					class="input is-small"
					:disabled="disabled"
					:placeholder="$t('task.location.searchPlaceholder')"
					autocomplete="off"
					@input="onSearchInput"
					@keydown.esc="cancelEditing"
					@focus="onSearchInput"
					@blur="onBlur"
				>
				<span class="icon is-small is-left">
					<Icon icon="map-marker-alt" />
				</span>
			</div>

			<ul
				v-if="showDropdown && suggestions.length > 0"
				class="location-suggestions-menu"
				role="listbox"
			>
				<li
					v-for="(item, idx) in suggestions"
					:key="item.place_id || idx"
					class="location-suggestion-item"
					role="option"
					@mousedown.prevent="selectSuggestion(item)"
				>
					<div class="suggestion-title">
						{{ item.display_name }}
					</div>
					<div
						v-if="item.address && item.address !== item.display_name"
						class="suggestion-address"
					>
						{{ item.address }}
					</div>
				</li>
			</ul>
			<div
				v-else-if="showDropdown && isSearching"
				class="location-searching-hint"
			>
				{{ $t('task.location.searching') }}
			</div>
			<div
				v-else-if="showDropdown && searchQuery.trim().length >= 2 && suggestions.length === 0"
				class="location-searching-hint"
			>
				{{ $t('task.location.noResults') }}
			</div>
		</div>
	</div>
</template>

<script setup lang="ts">
import {computed, nextTick, onMounted, ref, watch} from 'vue'
import {useI18n} from 'vue-i18n'

import BaseButton from '@/components/base/BaseButton.vue'
import Icon from '@/components/misc/Icon'
import {
	deleteTaskLocation,
	getTaskLocation,
	saveTaskLocation,
	searchLocations,
	type GeocodeResult,
	type TaskLocation,
} from '@/client/queries/maps'
import {success} from '@/message'

const props = withDefaults(defineProps<{
	taskId: number
	disabled?: boolean
}>(), {
	disabled: false,
})

const emit = defineEmits<{
	'updated': [location: TaskLocation | null]
	'cleared': []
}>()

const {t} = useI18n()

const savedLocation = ref<TaskLocation | null>(null)
const searchQuery = ref('')
const suggestions = ref<GeocodeResult[]>([])
const isSearching = ref(false)
const showDropdown = ref(false)
const isEditing = ref(false)
const inputRef = ref<HTMLInputElement | null>(null)

let debounceTimer: ReturnType<typeof setTimeout> | null = null

const mapUrl = computed(() => {
	if (!savedLocation.value) return ''
	const {latitude, longitude} = savedLocation.value
	return `https://www.openstreetmap.org/?mlat=${latitude}&mlon=${longitude}#map=16/${latitude}/${longitude}`
})

async function loadLocation() {
	if (!props.taskId) return
	try {
		savedLocation.value = await getTaskLocation(props.taskId)
	} catch {
		savedLocation.value = null
	}
	emit('updated', savedLocation.value)
}

function onSearchInput() {
	if (debounceTimer) clearTimeout(debounceTimer)
	const q = searchQuery.value.trim()
	if (q.length < 2) {
		suggestions.value = []
		showDropdown.value = false
		return
	}
	isSearching.value = true
	showDropdown.value = true
	debounceTimer = setTimeout(async () => {
		try {
			suggestions.value = await searchLocations(q)
		} catch {
			suggestions.value = []
		} finally {
			isSearching.value = false
		}
	}, 350)
}

async function selectSuggestion(item: GeocodeResult) {
	try {
		const lat = typeof item.lat === 'number' ? item.lat : (item.latitude ?? 0)
		const lon = typeof item.lon === 'number' ? item.lon : (item.longitude ?? 0)
		const address = item.address || item.display_name
		const displayName = item.display_name || address
		const title = item.display_name && item.display_name !== address ? item.display_name : displayName
		const saved = await saveTaskLocation(props.taskId, {
			display_name: displayName,
			title,
			address,
			latitude: lat,
			longitude: lon,
		})
		savedLocation.value = saved
		suggestions.value = []
		searchQuery.value = ''
		showDropdown.value = false
		isEditing.value = false
		emit('updated', saved)
		success(t('task.location.saved'))
	} catch {
		// Ignore or handle error
	}
}

async function clearLocation() {
	try {
		await deleteTaskLocation(props.taskId)
		savedLocation.value = null
		isEditing.value = false
		emit('updated', null)
		emit('cleared')
		success(t('task.location.removed'))
	} catch {
		// Ignore
	}
}

function startEditing() {
	if (props.disabled) return
	searchQuery.value = savedLocation.value?.address ?? ''
	isEditing.value = true
	nextTick(() => {
		inputRef.value?.focus()
	})
}

function cancelEditing() {
	isEditing.value = false
	showDropdown.value = false
	suggestions.value = []
}

function onBlur() {
	setTimeout(() => {
		showDropdown.value = false
		if (savedLocation.value) {
			isEditing.value = false
		}
	}, 200)
}

function focus() {
	isEditing.value = true
	nextTick(() => {
		inputRef.value?.focus()
	})
}

defineExpose({
	focus,
})

watch(() => props.taskId, () => {
	loadLocation()
})

onMounted(() => {
	loadLocation()
})
</script>

<style scoped lang="scss">
.task-location-field {
	display: flex;
	align-items: center;
	inline-size: 100%;
}

.location-tag-container {
	display: inline-flex;
	max-inline-size: 100%;
}

.location-tag {
	display: inline-flex;
	align-items: center;
	max-inline-size: 100%;
	background: var(--grey-200);
	color: var(--text);
	border-radius: $radius;
	padding: 0.25rem 0.5rem;
	font-size: 0.825rem;
	gap: 0.35rem;

	&.is-clickable {
		cursor: pointer;

		&:hover {
			background: var(--grey-300);
		}
	}

	.location-tag-icon {
		display: inline-flex;
		align-items: center;
		color: var(--primary);
		flex-shrink: 0;
	}

	.location-tag-text {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-weight: 500;
	}

	.location-map-link {
		color: var(--text-light, var(--grey-500));
		display: inline-flex;
		align-items: center;
		font-size: 0.75rem;
		margin-inline-start: 0.15rem;

		&:hover {
			color: var(--primary);
		}
	}
}

.location-autocomplete {
	position: relative;
	inline-size: 100%;
}

.location-suggestions-menu {
	position: absolute;
	inset-block-start: calc(100% + 4px);
	inset-inline-start: 0;
	min-inline-size: 260px;
	max-inline-size: 340px;
	z-index: 100;
	max-block-size: 220px;
	overflow-y: auto;
	background: var(--scheme-main);
	border: 1px solid var(--border);
	border-radius: $radius;
	box-shadow: var(--shadow-md, 0 4px 14px rgba(0, 0, 0, 0.15));
	list-style: none;
	margin: 0;
	padding: 0.35rem 0;
}

.location-suggestion-item {
	padding: 0.5rem 0.75rem;
	cursor: pointer;
	text-align: start;
	border-block-end: 1px solid var(--border-light, var(--border));

	&:last-child {
		border-block-end: none;
	}

	&:hover,
	&:focus {
		background: var(--grey-100);
	}
}

.suggestion-title {
	font-size: 0.825rem;
	font-weight: 600;
	color: var(--text-strong, var(--text));
	white-space: nowrap;
	overflow: hidden;
	text-overflow: ellipsis;
}

.suggestion-address {
	font-size: 0.72rem;
	color: var(--text-light, var(--grey-500));
	white-space: nowrap;
	overflow: hidden;
	text-overflow: ellipsis;
	margin-block-start: 0.15rem;
}

.location-searching-hint {
	position: absolute;
	inset-block-start: calc(100% + 4px);
	inset-inline-start: 0;
	min-inline-size: 200px;
	z-index: 100;
	padding: 0.5rem 0.75rem;
	background: var(--scheme-main);
	border: 1px solid var(--border);
	border-radius: $radius;
	font-size: 0.8rem;
	color: var(--text-light, var(--grey-500));
	box-shadow: var(--shadow-md, 0 4px 14px rgba(0, 0, 0, 0.15));
}
</style>
