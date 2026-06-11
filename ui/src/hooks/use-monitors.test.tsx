import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { describe, expect, it, beforeEach, vi } from 'vitest'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useCreateRepositoryMonitor } from './use-monitors'

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
