import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import {
  useSessionList,
  useSessionListAll,
  maxSessionWalkPages,
  useSession,
  useDeleteSession,
  retryUnlessClientError,
} from './use-sessions'
import { ApiError } from '@/lib/api-client'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useAuthStore.setState({ token: 'test-token' })
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useSessionList', () => {
  it('returns session list from API', async () => {
    const { result } = renderHook(() => useSessionList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [], metadata: {} })
  })
})

describe('useSessionListAll', () => {
  it('reports when the bounded list walk stops before the collection ends', async () => {
    let calls = 0
    server.use(http.get('/api/v1/sessions', () => {
      calls += 1
      return HttpResponse.json({
        items: [{ id: `session-${calls}` }],
        metadata: { continue: `page-${calls}` },
      })
    }))

    const { result } = renderHook(() => useSessionListAll('100', false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(calls).toBe(maxSessionWalkPages)
    expect(result.current.data?.items).toHaveLength(maxSessionWalkPages)
    expect(result.current.data?.truncated).toBe(true)
    expect(result.current.data?.metadata.continue).toBe(`page-${maxSessionWalkPages}`)
  })
})

describe('useSession', () => {
  it('returns a single session by id', async () => {
    const { result } = renderHook(() => useSession('sess-1'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({
      name: 'sess-1',
      namespace: 'default',
      messageCount: '5',
    })
  })
})

describe('useDeleteSession', () => {
  it('deletes a session via mutation', async () => {
    const { result } = renderHook(() => useDeleteSession(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate('sess-1')
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})

describe('retryUnlessClientError', () => {
  it('retries transient failures and 408/429 but not other client errors', () => {
    expect(retryUnlessClientError(0, new Error('network'))).toBe(true)
    expect(retryUnlessClientError(0, new ApiError(500, 'boom'))).toBe(true)
    expect(retryUnlessClientError(0, new ApiError(408, 'timeout'))).toBe(true)
    expect(retryUnlessClientError(0, new ApiError(429, 'throttled'))).toBe(true)
    expect(retryUnlessClientError(0, new ApiError(403, 'forbidden'))).toBe(false)
    expect(retryUnlessClientError(0, new ApiError(404, 'missing'))).toBe(false)
    expect(retryUnlessClientError(3, new ApiError(429, 'throttled'))).toBe(false)
  })
})
