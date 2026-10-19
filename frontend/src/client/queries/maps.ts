import {getApiRootUrl} from '@/helpers/apiUrl'
import {getToken} from '@/helpers/auth'

export interface TaskLocation {
	task_id: number
	title?: string
	address: string
	latitude: number
	longitude: number
	created?: string
	updated?: string
}

export interface GeocodeResult {
	place_id?: string
	display_name: string
	address?: string
	latitude?: number
	longitude?: number
	lat?: number
	lon?: number
	provider?: string
	map_url?: string
}

function getAuthHeaders(): HeadersInit {
	const headers: Record<string, string> = {
		'Content-Type': 'application/json',
	}
	const token = getToken()
	if (token) {
		headers.Authorization = `Bearer ${token}`
	}
	return headers
}

export const mapsQueryKeys = {
	all: ['maps'] as const,
	task: (taskId: number) => ['maps', 'task', taskId] as const,
	project: (projectId: number) => ['maps', 'project', projectId] as const,
}

export async function searchLocations(query: string, signal?: AbortSignal): Promise<GeocodeResult[]> {
	if (!query.trim()) {
		return []
	}
	const rootUrl = getApiRootUrl()
	const url = `${rootUrl}/api/v1/plugins/maps/geocode?q=${encodeURIComponent(query)}`
	const res = await fetch(url, {
		headers: getAuthHeaders(),
		credentials: 'include',
		signal,
	})
	if (!res.ok) {
		throw new Error(`Failed to search location: ${res.statusText}`)
	}
	return res.json()
}

export async function getTaskLocation(taskId: number): Promise<TaskLocation | null> {
	const rootUrl = getApiRootUrl()
	const url = `${rootUrl}/api/v1/plugins/maps/tasks/${taskId}/location`
	const res = await fetch(url, {
		headers: getAuthHeaders(),
		credentials: 'include',
	})
	if (res.status === 404) {
		return null
	}
	if (!res.ok) {
		throw new Error(`Failed to fetch task location: ${res.statusText}`)
	}
	return res.json()
}

export async function saveTaskLocation(
	taskId: number,
	loc: { address: string; latitude: number; longitude: number; title?: string; display_name?: string },
): Promise<TaskLocation> {
	const rootUrl = getApiRootUrl()
	const url = `${rootUrl}/api/v1/plugins/maps/tasks/${taskId}/location`
	const displayName = loc.display_name || loc.title || loc.address
	const payload = {
		display_name: displayName,
		title: loc.title || displayName,
		address: loc.address || displayName,
		latitude: loc.latitude,
		longitude: loc.longitude,
	}
	const res = await fetch(url, {
		method: 'POST',
		headers: getAuthHeaders(),
		credentials: 'include',
		body: JSON.stringify(payload),
	})
	if (!res.ok) {
		throw new Error(`Failed to save task location: ${res.statusText}`)
	}
	return res.json()
}

export async function deleteTaskLocation(taskId: number): Promise<void> {
	const rootUrl = getApiRootUrl()
	const url = `${rootUrl}/api/v1/plugins/maps/tasks/${taskId}/location`
	const res = await fetch(url, {
		method: 'DELETE',
		headers: getAuthHeaders(),
		credentials: 'include',
	})
	if (!res.ok && res.status !== 404) {
		throw new Error(`Failed to delete task location: ${res.statusText}`)
	}
}

export async function getProjectLocations(projectId: number): Promise<TaskLocation[]> {
	const rootUrl = getApiRootUrl()
	const url = `${rootUrl}/api/v1/plugins/maps/projects/${projectId}/locations`
	const res = await fetch(url, {
		headers: getAuthHeaders(),
		credentials: 'include',
	})
	if (!res.ok) {
		throw new Error(`Failed to fetch project locations: ${res.statusText}`)
	}
	const data = await res.json()
	return Array.isArray(data) ? data : []
}

export interface MapsConfig {
	provider: string
	supports_autocomplete: boolean
}

export async function getMapsConfig(): Promise<MapsConfig> {
	try {
		const rootUrl = getApiRootUrl()
		const url = `${rootUrl}/api/v1/plugins/maps/config`
		const res = await fetch(url, {
			headers: getAuthHeaders(),
			credentials: 'include',
		})
		if (!res.ok) {
			return {provider: 'photon', supports_autocomplete: false}
		}
		return await res.json()
	} catch {
		return {provider: 'photon', supports_autocomplete: false}
	}
}
