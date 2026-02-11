import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useChildTasks } from './use-child-tasks'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useChildTasks', () => {
  it('returns children from API', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/children', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'child-1', namespace: 'default' },
              spec: { type: 'ai' },
              status: { phase: 'Succeeded' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    const { result } = renderHook(() => useChildTasks('parent-task'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.items).toHaveLength(1)
    expect(result.current.data?.items[0].metadata.name).toBe('child-1')
  })

  it('returns empty list when no children', async () => {
    const { result } = renderHook(() => useChildTasks('parent-task'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [], metadata: {} })
  })

  it('does not fetch when disabled', () => {
    const { result } = renderHook(() => useChildTasks('parent-task', false), { wrapper: createWrapper() })
    expect(result.current.isFetching).toBe(false)
    expect(result.current.data).toBeUndefined()
  })
})
