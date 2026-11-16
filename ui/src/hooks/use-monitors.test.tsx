import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useCreateRepositoryMonitor, useRepositoryMonitor } from './use-monitors'

function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
}

function createWrapper(queryClient: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useCreateRepositoryMonitor', () => {
  it('posts the create request and invalidates monitor list/detail queries', async () => {
    const queryClient = createTestQueryClient()
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
    let receivedBody: unknown

    server.use(
      http.post('/api/v1/monitors/repositories', async ({ request }) => {
        receivedBody = await request.json()
        return HttpResponse.json({
          metadata: { name: 'example-app', namespace: 'default' },
          spec: { repoURL: 'https://github.com/example/app' },
        }, { status: 201 })
      }),
    )

    const body = {
      name: 'example-app',
      namespace: 'default',
      spec: {
        repoURL: 'https://github.com/example/app',
        agents: { reviewer: { name: 'repo-reviewer' } },
      },
    }

    const { result } = renderHook(() => useCreateRepositoryMonitor(), { wrapper: createWrapper(queryClient) })

    await act(async () => {
      await result.current.mutateAsync(body)
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(receivedBody).toEqual(body)
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['monitors', 'repositories'] })
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['monitors', 'repositories', 'default'] })
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['monitors', 'repository', 'default', 'example-app'] })
  })
})

describe('useRepositoryMonitor on 404', () => {
  const notFound = () => HttpResponse.json({ error: { code: 404, message: 'repository monitor not found' } }, { status: 404 })

  async function advance(ms: number) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms)
    })
  }

  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('does not retry, but keeps polling a monitor never seen', async () => {
    let calls = 0
    server.use(http.get('/api/v1/monitors/repositories/new-monitor', () => {
      calls += 1
      return notFound()
    }))

    // The app's default of one retry, which the hook must override for a 404.
    const appClient = new QueryClient({ defaultOptions: { queries: { retry: 1, gcTime: 0 } } })
    renderHook(() => useRepositoryMonitor('new-monitor'), { wrapper: createWrapper(appClient) })

    await advance(9_000)
    expect(calls).toBe(1)
    // The create form opens a monitor right away; it can 404 until the API cache catches up.
    await advance(1_500)
    expect(calls).toBe(2)
  })

  it('stops polling a monitor that 404s after it loaded', async () => {
    let calls = 0
    server.use(http.get('/api/v1/monitors/repositories/deleted-monitor', () => {
      calls += 1
      return calls === 1
        ? HttpResponse.json({ metadata: { name: 'deleted-monitor' }, spec: { repoURL: 'https://github.com/org/repo' } })
        : notFound()
    }))

    renderHook(() => useRepositoryMonitor('deleted-monitor'), { wrapper: createWrapper(createTestQueryClient()) })

    await advance(10_500)
    expect(calls).toBe(2)
    await advance(60_000)
    expect(calls).toBe(2)
  })
})
