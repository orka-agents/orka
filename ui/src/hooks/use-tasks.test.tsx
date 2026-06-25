import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import {
  useTaskList,
  useTask,
  useTaskResult,
  useCreateTask,
  useDeleteTask,
  useTaskEvents,
} from './use-tasks'

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

describe('useTaskList', () => {
  it('returns task list from API', async () => {
    const { result } = renderHook(() => useTaskList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [], metadata: {} })
  })
})

describe('useTask', () => {
  it('returns a single task by id', async () => {
    const { result } = renderHook(() => useTask('my-task'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({
      metadata: { name: 'my-task', namespace: 'default' },
      status: { phase: 'Succeeded' },
    })
  })
})

describe('useTaskResult', () => {
  it('starts disabled and returns result on refetch', async () => {
    const { result } = renderHook(() => useTaskResult('my-task'), { wrapper: createWrapper() })
    // Should not fetch automatically
    expect(result.current.isFetching).toBe(false)
    expect(result.current.data).toBeUndefined()

    // Trigger manual refetch
    await act(async () => {
      await result.current.refetch()
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ result: 'task output' })
  })
})



describe('useTaskEvents', () => {
  it('fetches pages until the initial latest sequence is covered', async () => {
    const requests: string[] = []
    server.use(
      http.get('/api/v1/tasks/:id/events', ({ request, params }) => {
        const url = new URL(request.url)
        requests.push(url.search)
        const after = url.searchParams.get('after')
        if (!after) {
          return HttpResponse.json({
            namespace: 'default',
            streamType: 'task',
            streamID: params.id,
            afterSeq: 0,
            latestSeq: 1001,
            events: [{
              id: 'default/task/my-task/1000',
              namespace: 'default',
              streamType: 'task',
              streamID: params.id,
              seq: 1000,
              type: 'ModelRequestCompleted',
              severity: 'info',
              inputTokens: 5,
              outputTokens: 7,
              createdAt: '2026-01-01T00:00:00Z',
            }],
          })
        }
        if (after === '1000') {
          return HttpResponse.json({
            namespace: 'default',
            streamType: 'task',
            streamID: params.id,
            afterSeq: 1000,
            latestSeq: 1001,
            events: [{
              id: 'default/task/my-task/1001',
              namespace: 'default',
              streamType: 'task',
              streamID: params.id,
              seq: 1001,
              type: 'ModelRequestCompleted',
              severity: 'info',
              inputTokens: 11,
              outputTokens: 13,
              createdAt: '2026-01-01T00:00:01Z',
            }],
          })
        }
        expect(after).toBe('1001')
        return HttpResponse.json({
          namespace: 'default',
          streamType: 'task',
          streamID: params.id,
          afterSeq: 1001,
          latestSeq: 1001,
          events: [],
        })
      }),
    )

    const { result } = renderHook(() => useTaskEvents('my-task'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(requests).toEqual([
      '?namespace=default&limit=1000',
      '?namespace=default&limit=1000&after=1000',
    ])
    expect(result.current.data?.latestSeq).toBe(1001)
    expect(result.current.data?.events.map((event) => event.seq)).toEqual([1000, 1001])

    await act(async () => {
      await result.current.refetch()
    })
    expect(requests).toEqual([
      '?namespace=default&limit=1000',
      '?namespace=default&limit=1000&after=1000',
      '?namespace=default&limit=1000&after=1001',
    ])
    expect(result.current.data?.events.map((event) => event.seq)).toEqual([1000, 1001])
  })
})


describe('useCreateTask', () => {
  it('creates a task via mutation', async () => {
    const { result } = renderHook(() => useCreateTask(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate({ type: 'container', image: 'alpine' })
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({ metadata: { name: 'new-task' } })
  })
})

describe('useDeleteTask', () => {
  it('deletes a task via mutation', async () => {
    const { result } = renderHook(() => useDeleteTask(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate('task-to-delete')
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
