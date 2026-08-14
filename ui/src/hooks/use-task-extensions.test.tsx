// Tests for task plan/children reads and server-side chat cancellation.
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { http, HttpResponse } from 'msw'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { ApiError } from '@/lib/api-client'
import { useTaskChildren, useTaskPlan } from './use-tasks'
import { useCancelChatSession } from './use-chat'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useTaskPlan', () => {
  it('returns plan state', async () => {
    const { result } = renderHook(() => useTaskPlan('t1', false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.progressPct).toBe(50)
  })

  it('stops on 501 when the plan store is disabled', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/plan', () =>
        HttpResponse.json({ error: { code: 501, message: 'plan store not configured' } }, { status: 501 }),
      ),
    )
    const { result } = renderHook(() => useTaskPlan('t1', false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isError).toBe(true))
    expect(result.current.error).toBeInstanceOf(ApiError)
    expect((result.current.error as ApiError).status).toBe(501)
  })
})

describe('useTaskChildren', () => {
  it('lists child tasks', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/children', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'child-1', namespace: 'default' },
              spec: { type: 'ai' },
              status: { phase: 'Running' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    const { result } = renderHook(() => useTaskChildren('t1', false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.items[0].metadata.name).toBe('child-1')
  })
})

describe('useCancelChatSession', () => {
  it('deletes the chat session server-side', async () => {
    let deleted = ''
    server.use(
      http.delete('/api/v1/chat/:sessionId', ({ params }) => {
        deleted = String(params.sessionId)
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const { result } = renderHook(() => useCancelChatSession(), { wrapper: createWrapper() })
    result.current.mutate('sess-1')
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(deleted).toBe('sess-1')
  })

  it('surfaces 409 when unsettled work pins the session', async () => {
    server.use(
      http.delete('/api/v1/chat/:sessionId', () =>
        HttpResponse.json({ error: { code: 409, message: 'session has unsettled work' } }, { status: 409 }),
      ),
    )
    const { result } = renderHook(() => useCancelChatSession(), { wrapper: createWrapper() })
    result.current.mutate('sess-1')
    await waitFor(() => expect(result.current.isError).toBe(true))
    expect((result.current.error as ApiError).status).toBe(409)
  })
})

describe('gateway classes', () => {
  it('lists cluster gateway classes', async () => {
    const { useGatewayClasses } = await import('./use-gateways')
    server.use(
      http.get('/api/v1/gatewayclasses', () =>
        HttpResponse.json({
          items: [{ metadata: { name: 'slack' }, spec: { category: 'chat' }, status: { accepted: true } }],
          metadata: {},
        }),
      ),
    )
    const { result } = renderHook(() => useGatewayClasses(false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.[0].metadata.name).toBe('slack')
  })
})
