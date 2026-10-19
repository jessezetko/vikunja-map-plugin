import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {flushPromises, mount} from '@vue/test-utils'
import LocationSearchField from '@/components/tasks/partials/LocationSearchField.vue'
import * as mapsApi from '@/client/queries/maps'
import * as message from '@/message'

vi.mock('vue-i18n', () => ({
	useI18n: () => ({
		t: (key: string) => key,
	}),
}))

vi.mock('@/message', () => ({
	error: vi.fn(),
	success: vi.fn(),
}))

describe('LocationSearchField.vue', () => {
	beforeEach(() => {
		vi.useFakeTimers()
		vi.clearAllMocks()
		vi.spyOn(mapsApi, 'getTaskLocation').mockResolvedValue(null)
		vi.spyOn(mapsApi, 'getMapsConfig').mockResolvedValue({
			provider: 'photon',
			supports_autocomplete: true,
		})
	})

	afterEach(() => {
		vi.useRealTimers()
	})

	it('debounces keystrokes and calls searchLocations in autocomplete mode', async () => {
		const searchSpy = vi.spyOn(mapsApi, 'searchLocations').mockResolvedValue([
			{
				place_id: '1',
				display_name: 'Berlin, Germany',
				latitude: 52.52,
				longitude: 13.405,
				provider: 'photon',
			},
		])

		const wrapper = mount(LocationSearchField, {
			props: {taskId: 123},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {Icon: true, BaseButton: true},
			},
		})

		await flushPromises()

		const input = wrapper.find('input')
		await input.setValue('Ber')

		// Should not have fired immediately
		expect(searchSpy).not.toHaveBeenCalled()

		// Advance debounce timer (400ms)
		vi.advanceTimersByTime(400)
		await flushPromises()

		expect(searchSpy).toHaveBeenCalledTimes(1)
		expect(searchSpy).toHaveBeenCalledWith('Ber', expect.any(AbortSignal))
	})

	it('disables search-as-you-type when provider is Nominatim and requires explicit search', async () => {
		vi.spyOn(mapsApi, 'getMapsConfig').mockResolvedValue({
			provider: 'nominatim',
			supports_autocomplete: false,
		})

		const searchSpy = vi.spyOn(mapsApi, 'searchLocations').mockResolvedValue([])

		const wrapper = mount(LocationSearchField, {
			props: {taskId: 123},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {Icon: true, BaseButton: true},
			},
		})

		await flushPromises()

		const input = wrapper.find('input')
		await input.setValue('London')

		// Advance timer: should NOT search as-you-type under Nominatim
		vi.advanceTimersByTime(500)
		await flushPromises()

		expect(searchSpy).not.toHaveBeenCalled()

		// Press Enter to trigger explicit search
		await input.trigger('keydown.enter')
		await flushPromises()

		expect(searchSpy).toHaveBeenCalledTimes(1)
		expect(searchSpy).toHaveBeenCalledWith('London', expect.any(AbortSignal))
	})

	it('shows error notification when search fails', async () => {
		vi.spyOn(mapsApi, 'searchLocations').mockRejectedValue(new Error('Network error'))

		const wrapper = mount(LocationSearchField, {
			props: {taskId: 123},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {Icon: true, BaseButton: true},
			},
		})

		await flushPromises()

		const input = wrapper.find('input')
		await input.setValue('Paris')

		vi.advanceTimersByTime(400)
		await flushPromises()

		expect(message.error).toHaveBeenCalledWith('task.location.searchError')
	})

	it('shows error notification when saving location fails', async () => {
		vi.spyOn(mapsApi, 'searchLocations').mockResolvedValue([
			{
				place_id: '1',
				display_name: 'Tokyo, Japan',
				latitude: 35.6762,
				longitude: 139.6503,
				provider: 'photon',
			},
		])
		vi.spyOn(mapsApi, 'saveTaskLocation').mockRejectedValue(new Error('DB error'))

		const wrapper = mount(LocationSearchField, {
			props: {taskId: 123},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {Icon: true, BaseButton: true},
			},
		})

		await flushPromises()

		const input = wrapper.find('input')
		await input.setValue('Tokyo')

		vi.advanceTimersByTime(400)
		await flushPromises()

		const suggestion = wrapper.find('.location-suggestion-item')
		expect(suggestion.exists()).toBe(true)

		await suggestion.trigger('mousedown')
		await flushPromises()

		expect(message.error).toHaveBeenCalledWith('task.location.saveError')
	})

	it('shows error notification when deleting location fails', async () => {
		vi.spyOn(mapsApi, 'getTaskLocation').mockResolvedValue({
			task_id: 123,
			address: 'Existing Place',
			latitude: 10,
			longitude: 20,
		})
		vi.spyOn(mapsApi, 'deleteTaskLocation').mockRejectedValue(new Error('Delete error'))

		const wrapper = mount(LocationSearchField, {
			props: {taskId: 123},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {Icon: true, BaseButton: true},
			},
		})

		await flushPromises()

		const deleteBtn = wrapper.find('.delete')
		expect(deleteBtn.exists()).toBe(true)

		await deleteBtn.trigger('click')
		await flushPromises()

		expect(message.error).toHaveBeenCalledWith('task.location.deleteError')
	})

	it('fails closed and disables autocomplete when getMapsConfig rejects', async () => {
		vi.spyOn(mapsApi, 'getMapsConfig').mockRejectedValue(new Error('Network error'))
		const searchSpy = vi.spyOn(mapsApi, 'searchLocations').mockResolvedValue([])

		const wrapper = mount(LocationSearchField, {
			props: {taskId: 123},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {Icon: true, BaseButton: true},
			},
		})

		await flushPromises()

		const input = wrapper.find('input')
		await input.setValue('Berlin')

		// Should not search as-you-type because autocomplete must fail closed
		vi.advanceTimersByTime(500)
		await flushPromises()

		expect(searchSpy).not.toHaveBeenCalled()

		// Explicit search via Enter key still triggers search
		await input.trigger('keydown.enter')
		await flushPromises()

		expect(searchSpy).toHaveBeenCalledTimes(1)
		expect(searchSpy).toHaveBeenCalledWith('Berlin', expect.any(AbortSignal))
	})

	it('fails closed and disables autocomplete when getMapsConfig returns non-OK response', async () => {
		vi.spyOn(mapsApi, 'getMapsConfig').mockResolvedValue({
			provider: 'photon',
			supports_autocomplete: false,
		})
		const searchSpy = vi.spyOn(mapsApi, 'searchLocations').mockResolvedValue([])

		const wrapper = mount(LocationSearchField, {
			props: {taskId: 123},
			global: {
				mocks: {$t: (k: string) => k},
				stubs: {Icon: true, BaseButton: true},
			},
		})

		await flushPromises()

		const input = wrapper.find('input')
		await input.setValue('Munich')

		vi.advanceTimersByTime(500)
		await flushPromises()

		expect(searchSpy).not.toHaveBeenCalled()

		await input.trigger('keydown.enter')
		await flushPromises()

		expect(searchSpy).toHaveBeenCalledTimes(1)
		expect(searchSpy).toHaveBeenCalledWith('Munich', expect.any(AbortSignal))
	})
})

describe('getMapsConfig() fail-closed fallback', () => {
	const originalFetch = global.fetch

	afterEach(() => {
		global.fetch = originalFetch
		vi.restoreAllMocks()
	})

	it('returns supports_autocomplete: false when fetch rejects with network error', async () => {
		vi.restoreAllMocks()
		global.fetch = vi.fn().mockRejectedValue(new Error('Network connection failed'))
		const config = await mapsApi.getMapsConfig()
		expect(config.supports_autocomplete).toBe(false)
	})

	it('returns supports_autocomplete: false when fetch returns non-OK HTTP status', async () => {
		vi.restoreAllMocks()
		global.fetch = vi.fn().mockResolvedValue({
			ok: false,
			status: 502,
			statusText: 'Bad Gateway',
		} as Response)
		const config = await mapsApi.getMapsConfig()
		expect(config.supports_autocomplete).toBe(false)
	})
})

