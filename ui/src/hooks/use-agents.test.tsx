import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import {
  useAgentList,
  useAgent,
  useCreateAgent,
  useDeleteAgent,
} from './use-agents'

function createWrapper(queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
})) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useAuthStore.setState({ token: 'test-token' })
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useAgentList', () => {
  it('returns agent list from API', async () => {
    const { result } = renderHook(() => useAgentList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [], metadata: {} })
  })

  it('polls so active task counts and readiness stay current', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      let requests = 0
      server.use(http.get('/api/v1/agents', () => {
        requests += 1
        return HttpResponse.json({ items: [], metadata: {} })
      }))
      const { result } = renderHook(() => useAgentList(), { wrapper: createWrapper() })
      await waitFor(() => expect(result.current.isSuccess).toBe(true))
      expect(requests).toBe(1)
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000)
      })
      await waitFor(() => expect(requests).toBe(2))
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('useAgent', () => {
  it('returns a single agent by name', async () => {
    const { result } = renderHook(() => useAgent('my-agent'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({
      metadata: { name: 'my-agent', namespace: 'default' },
    })
  })

  it('polls the agent but stops once it is gone', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      let requests = 0
      let gone = false
      server.use(http.get('/api/v1/agents/:name', ({ params }) => {
        requests += 1
        if (gone) return HttpResponse.json({ error: { code: 404, message: 'not found' } }, { status: 404 })
        return HttpResponse.json({ metadata: { name: params.name, namespace: 'default' }, spec: {} })
      }))
      const { result } = renderHook(() => useAgent('my-agent'), { wrapper: createWrapper() })
      await waitFor(() => expect(result.current.isSuccess).toBe(true))
      gone = true
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000)
      })
      await waitFor(() => expect(result.current.isError).toBe(true))
      const afterGone = requests
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000)
      })
      expect(requests).toBe(afterGone)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('useCreateAgent', () => {
  it('creates an agent via mutation', async () => {
    const { result } = renderHook(() => useCreateAgent(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate({ name: 'new-agent' })
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({ metadata: { name: 'new-agent' } })
  })
})

describe('useDeleteAgent', () => {
  it('deletes an agent via mutation', async () => {
    const { result } = renderHook(() => useDeleteAgent(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate('my-agent')
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
