import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {flushPromises, mount} from '@vue/test-utils'
import {ref} from 'vue'
import L from 'leaflet'

import ProjectViewMap from '@/components/project/views/ProjectViewMap.vue'
import * as mapsApi from '@/client/queries/maps'
import type {TaskLocation} from '@/client/queries/maps'

const isDarkRef = ref(false)
const tasksRef = ref<Array<{id: number; title: string; hex_color?: string}>>([])

vi.mock('vue-router', async importOriginal => ({
	...await importOriginal<typeof import('vue-router')>(),
	useRouter: () => ({
		push: vi.fn(),
	}),
}))

vi.mock('vue-i18n', async importOriginal => ({
	...await importOriginal<typeof import('vue-i18n')>(),
	useI18n: () => ({
		t: (key: string) => key,
	}),
}))

vi.mock('@/message', () => ({
	error: vi.fn(),
	success: vi.fn(),
}))

vi.mock('@/composables/useColorScheme', () => ({
	useColorScheme: () => ({
		isDark: isDarkRef,
	}),
}))

vi.mock('@/composables/useTaskList', () => ({
	useTaskList: () => ({
		tasks: tasksRef,
	}),
}))

describe('ProjectViewMap.vue - Custom Work Item Pin Colors', () => {
	let markerSpy: ReturnType<typeof vi.spyOn>
	let wrapper: ReturnType<typeof mount> | null = null

	beforeEach(() => {
		vi.clearAllMocks()
		isDarkRef.value = false
		tasksRef.value = []
		markerSpy = vi.spyOn(L, 'marker')
	})

	afterEach(() => {
		if (wrapper) {
			wrapper.unmount()
			wrapper = null
		}
		markerSpy.mockRestore()
	})

	it('uses parent work item custom hex color with hash on map marker pin', async () => {
		tasksRef.value = [
			{id: 101, title: 'Red Task', hex_color: '#e63946'},
		]

		const locations: TaskLocation[] = [
			{id: 1, task_id: 101, project_id: 1, title: 'Location 1', address: 'Addr 1', latitude: 10, longitude: 20},
		]
		vi.spyOn(mapsApi, 'getProjectLocations').mockResolvedValue(locations)

		wrapper = mount(ProjectViewMap, {
			props: {projectId: 1, viewId: 1},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {
					Icon: true,
					BaseButton: true,
					ProjectWrapper: {template: '<div><slot name="header"/><slot/></div>'},
				},
			},
		})

		await flushPromises()

		expect(markerSpy).toHaveBeenCalled()
		const lastCall = markerSpy.mock.calls[markerSpy.mock.calls.length - 1]
		const iconOption = lastCall[1]?.icon as L.DivIcon
		expect(iconOption).toBeDefined()
		expect(iconOption.options.html).toContain('background-color: #e63946;')
	})

	it('normalizes hex colors without leading hash and applies them to pin', async () => {
		tasksRef.value = [
			{id: 102, title: 'Green Task', hex_color: '2a9d8f'},
		]

		const locations: TaskLocation[] = [
			{id: 2, task_id: 102, project_id: 1, title: 'Location 2', address: 'Addr 2', latitude: 12, longitude: 22},
		]
		vi.spyOn(mapsApi, 'getProjectLocations').mockResolvedValue(locations)

		wrapper = mount(ProjectViewMap, {
			props: {projectId: 1, viewId: 1},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {
					Icon: true,
					BaseButton: true,
					ProjectWrapper: {template: '<div><slot name="header"/><slot/></div>'},
				},
			},
		})

		await flushPromises()

		expect(markerSpy).toHaveBeenCalled()
		const lastCall = markerSpy.mock.calls[markerSpy.mock.calls.length - 1]
		const iconOption = lastCall[1]?.icon as L.DivIcon
		expect(iconOption).toBeDefined()
		expect(iconOption.options.html).toContain('background-color: #2a9d8f;')
	})

	it('falls back to light theme primary color when task has no custom color', async () => {
		isDarkRef.value = false
		tasksRef.value = [
			{id: 103, title: 'Default Color Task', hex_color: ''},
		]

		const locations: TaskLocation[] = [
			{id: 3, task_id: 103, project_id: 1, title: 'Location 3', address: 'Addr 3', latitude: 14, longitude: 24},
		]
		vi.spyOn(mapsApi, 'getProjectLocations').mockResolvedValue(locations)

		wrapper = mount(ProjectViewMap, {
			props: {projectId: 1, viewId: 1},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {
					Icon: true,
					BaseButton: true,
					ProjectWrapper: {template: '<div><slot name="header"/><slot/></div>'},
				},
			},
		})

		await flushPromises()

		expect(markerSpy).toHaveBeenCalled()
		const lastCall = markerSpy.mock.calls[markerSpy.mock.calls.length - 1]
		const iconOption = lastCall[1]?.icon as L.DivIcon
		expect(iconOption).toBeDefined()
		expect(iconOption.options.html).toContain('background-color: #1976d2;')
	})

	it('falls back to dark theme primary color when dark mode is active and task has no custom color', async () => {
		isDarkRef.value = true
		tasksRef.value = [
			{id: 104, title: 'Dark Default Task'},
		]

		const locations: TaskLocation[] = [
			{id: 4, task_id: 104, project_id: 1, title: 'Location 4', address: 'Addr 4', latitude: 16, longitude: 26},
		]
		vi.spyOn(mapsApi, 'getProjectLocations').mockResolvedValue(locations)

		wrapper = mount(ProjectViewMap, {
			props: {projectId: 1, viewId: 1},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {
					Icon: true,
					BaseButton: true,
					ProjectWrapper: {template: '<div><slot name="header"/><slot/></div>'},
				},
			},
		})

		await flushPromises()

		expect(markerSpy).toHaveBeenCalled()
		const lastCall = markerSpy.mock.calls[markerSpy.mock.calls.length - 1]
		const iconOption = lastCall[1]?.icon as L.DivIcon
		expect(iconOption).toBeDefined()
		expect(iconOption.options.html).toContain('background-color: #3a86ff;')
	})

	it('falls back safely to default theme color if custom color is malformed or invalid', async () => {
		isDarkRef.value = false
		tasksRef.value = [
			{id: 105, title: 'Invalid Color Task', hex_color: 'invalid; color: red'},
		]

		const locations: TaskLocation[] = [
			{id: 5, task_id: 105, project_id: 1, title: 'Location 5', address: 'Addr 5', latitude: 18, longitude: 28},
		]
		vi.spyOn(mapsApi, 'getProjectLocations').mockResolvedValue(locations)

		wrapper = mount(ProjectViewMap, {
			props: {projectId: 1, viewId: 1},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {
					Icon: true,
					BaseButton: true,
					ProjectWrapper: {template: '<div><slot name="header"/><slot/></div>'},
				},
			},
		})

		await flushPromises()

		expect(markerSpy).toHaveBeenCalled()
		const lastCall = markerSpy.mock.calls[markerSpy.mock.calls.length - 1]
		const iconOption = lastCall[1]?.icon as L.DivIcon
		expect(iconOption).toBeDefined()
		expect(iconOption.options.html).toContain('background-color: #1976d2;')
	})
})
