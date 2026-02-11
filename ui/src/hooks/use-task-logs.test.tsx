import { describe, it, expect, beforeEach, vi } from 'vitest'
import { renderHook, waitFor, act } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useTaskLogs } from './use-task-logs'

describe('useTaskLogs', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    vi.restoreAllMocks()
  })

  it('fetches logs with correct URL and auth header', async () => {
    const mockResponse = new Response('line1\nline2\nline3\n', {
      status: 200,
      headers: { 'Content-Type': 'text/plain' },
    })
    // Force no body reader to use text() path
    Object.defineProperty(mockResponse, 'body', { value: null })
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(mockResponse)

    const { result } = renderHook(() => useTaskLogs('my-task'))

    await waitFor(() => {
      expect(result.current.logs.length).toBeGreaterThan(0)
    })

    expect(fetchSpy).toHaveBeenCalledWith(
      '/api/v1/tasks/my-task/logs?namespace=default',
      expect.objectContaining({
        headers: { Authorization: 'Bearer test-token' },
      })
    )
    expect(result.current.logs).toEqual(['line1', 'line2', 'line3'])
  })

  it('handles fetch errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(null, { status: 500, statusText: 'Internal Server Error' })
    )

    const { result } = renderHook(() => useTaskLogs('bad-task'))

    await waitFor(() => {
      expect(result.current.error).toBe('Failed to fetch logs: Internal Server Error')
    })
    expect(result.current.isStreaming).toBe(false)
  })

  it('clear function resets logs', async () => {
    const mockResponse = new Response('line1\n', { status: 200 })
    Object.defineProperty(mockResponse, 'body', { value: null })
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(mockResponse)

    const { result } = renderHook(() => useTaskLogs('my-task'))

    await waitFor(() => {
      expect(result.current.logs.length).toBe(1)
    })

    act(() => { result.current.clear() })

    expect(result.current.logs).toEqual([])
  })

  it('does not fetch when disabled', () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    renderHook(() => useTaskLogs('my-task', false))
    expect(fetchSpy).not.toHaveBeenCalled()
  })
})
