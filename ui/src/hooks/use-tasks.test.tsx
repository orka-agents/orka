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
  useTaskListAll,
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

function makeTask(
  name: string,
  namespace = 'default',
  uid: string | null = `uid-${namespace}-${name}`,
) {
  return {
    metadata: { name, namespace, ...(uid ? { uid } : {}) },
    spec: { type: 'container' as const },
    status: { phase: 'Running' as const },
  }
}

describe('useTaskList', () => {
  it('returns task list from API', async () => {
    const { result } = renderHook(() => useTaskList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [], metadata: {} })
  })

  it('stops when a continuation cursor does not advance', async () => {
    const seen: (string | null)[] = []
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        seen.push(cursor)
        if (!cursor) {
          return HttpResponse.json({
            items: [makeTask('task-1')],
            metadata: { continue: 'same-cursor' },
          })
        }
        return HttpResponse.json({
          items: [makeTask('task-2')],
          metadata: { continue: 'same-cursor' },
        })
      }),
    )

    const { result } = renderHook(() => useTaskList('25', false), {
      wrapper: createWrapper(),
    })
    await waitFor(() => expect(result.current.hasNextPage).toBe(true))
    expect(result.current.data?.items.map((task) => task.metadata.name)).toEqual([
      'task-1',
    ])

    await act(async () => {
      await result.current.fetchNextPage()
    })

    await waitFor(() => expect(seen).toEqual([null, 'same-cursor']))
    await waitFor(() =>
      expect(result.current.data?.items.map((task) => task.metadata.name)).toEqual([
        'task-1',
        'task-2',
      ]),
    )
    expect(result.current.hasNextPage).toBe(false)

    await act(async () => {
      await result.current.fetchNextPage()
    })
    expect(seen).toEqual([null, 'same-cursor'])
  })

  it('stops before following a continuation cursor cycle', async () => {
    const seen: (string | null)[] = []
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        seen.push(cursor)
        if (!cursor) {
          return HttpResponse.json({
            items: [makeTask('task-1')],
            metadata: { continue: 'cursor-a' },
          })
        }
        if (cursor === 'cursor-a') {
          return HttpResponse.json({
            items: [makeTask('task-2')],
            metadata: { continue: 'cursor-b' },
          })
        }
        expect(cursor).toBe('cursor-b')
        return HttpResponse.json({
          items: [makeTask('task-3')],
          metadata: { continue: 'cursor-a' },
        })
      }),
    )

    const { result } = renderHook(() => useTaskList('25', false), {
      wrapper: createWrapper(),
    })
    await waitFor(() => expect(result.current.hasNextPage).toBe(true))
    expect(result.current.data?.items).toHaveLength(1)

    await act(async () => {
      await result.current.fetchNextPage()
    })
    await waitFor(() => expect(result.current.data?.items).toHaveLength(2))
    expect(result.current.hasNextPage).toBe(true)

    await act(async () => {
      await result.current.fetchNextPage()
    })
    await waitFor(() => expect(result.current.data?.items).toHaveLength(3))
    expect(result.current.hasNextPage).toBe(false)

    await act(async () => {
      await result.current.fetchNextPage()
    })
    expect(seen).toEqual([null, 'cursor-a', 'cursor-b'])
  })

  it('deduplicates overlapping task rows across pages', async () => {
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        if (!cursor) {
          return HttpResponse.json({
            items: [
              makeTask('task-by-uid', 'default', 'shared-uid'),
              makeTask('task-by-name', 'default', null),
            ],
            metadata: { continue: 'page-2' },
          })
        }
        return HttpResponse.json({
          items: [
            makeTask('task-by-uid', 'default', 'shared-uid'),
            makeTask('task-by-name', 'default', null),
            makeTask('task-3'),
          ],
          metadata: {},
        })
      }),
    )

    const { result } = renderHook(() => useTaskList('25', false), {
      wrapper: createWrapper(),
    })
    await waitFor(() => expect(result.current.hasNextPage).toBe(true))
    expect(result.current.data?.items).toHaveLength(2)

    await act(async () => {
      await result.current.fetchNextPage()
    })

    await waitFor(() =>
      expect(result.current.data?.items.map((task) => task.metadata.name)).toEqual([
        'task-by-uid',
        'task-by-name',
        'task-3',
      ]),
    )
  })

  it('resets pagination when the namespace or page-size filter changes', async () => {
    const requests: Array<{
      namespace: string | null
      limit: string | null
      cursor: string | null
    }> = []
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const url = new URL(request.url)
        const namespace = url.searchParams.get('namespace')
        const limit = url.searchParams.get('limit')
        const cursor = url.searchParams.get('continue')
        requests.push({ namespace, limit, cursor })
        return HttpResponse.json({
          items: [
            makeTask(
              `${namespace}-${limit}-${cursor ?? 'first'}`,
              namespace ?? 'default',
            ),
          ],
          metadata: cursor ? {} : { continue: 'next-page' },
        })
      }),
    )

    const { result, rerender } = renderHook(
      ({ limit }) => useTaskList(limit, false),
      { initialProps: { limit: '25' }, wrapper: createWrapper() },
    )
    await waitFor(() => expect(result.current.hasNextPage).toBe(true))
    expect(result.current.data?.items).toHaveLength(1)

    await act(async () => {
      await result.current.fetchNextPage()
    })
    await waitFor(() => expect(result.current.data?.items).toHaveLength(2))

    act(() => {
      useUIStore.setState({ namespace: 'production' })
    })
    await waitFor(() =>
      expect(result.current.data?.items[0]?.metadata.name).toBe(
        'production-25-first',
      ),
    )

    rerender({ limit: '50' })
    await waitFor(() =>
      expect(result.current.data?.items[0]?.metadata.name).toBe(
        'production-50-first',
      ),
    )

    expect(requests).toEqual([
      { namespace: 'default', limit: '25', cursor: null },
      { namespace: 'default', limit: '25', cursor: 'next-page' },
      { namespace: 'production', limit: '25', cursor: null },
      { namespace: 'production', limit: '50', cursor: null },
    ])
  })
})

describe('useTaskListAll', () => {
  it('follows continue tokens and returns all task pages', async () => {
    const seen: (string | null)[] = []
    server.use(http.get('/api/v1/tasks', ({ request }) => {
      const token = new URL(request.url).searchParams.get('continue')
      seen.push(token)
      if (!token) {
        return HttpResponse.json({ items: [], metadata: { continue: 'next-page' } })
      }
      return HttpResponse.json({
        items: [{ metadata: { name: 'late-running', namespace: 'default', uid: 'late' }, spec: { type: 'agent' }, status: { phase: 'Running' } }],
        metadata: {},
      })
    }))

    const { result } = renderHook(() => useTaskListAll('100', false), { wrapper: createWrapper() })

    await waitFor(() => expect(result.current.data?.items[0]?.metadata.name).toBe('late-running'))
    expect(seen).toEqual([null, 'next-page'])
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

  it('uses the supplied refetch interval for task detail polling', async () => {
    let calls = 0
    server.use(http.get('/api/v1/tasks/poll-task', () => {
      calls += 1
      return HttpResponse.json({
        metadata: { name: 'poll-task', namespace: 'default', uid: 'uid-poll' },
        spec: { type: 'container', image: 'alpine' },
        status: { phase: 'Running' },
      })
    }))

    renderHook(() => useTask('poll-task', 20), { wrapper: createWrapper() })

    await waitFor(() => expect(calls).toBeGreaterThan(1))
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

  it('does not advance cursor past retained events when latest grows mid-fetch', async () => {
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
            latestSeq: 2000,
            events: [{
              id: 'default/task/my-task/1999',
              namespace: 'default',
              streamType: 'task',
              streamID: params.id,
              seq: 1999,
              type: 'ModelRequestCompleted',
              severity: 'info',
              createdAt: '2026-01-01T00:00:00Z',
            }],
          })
        }
        if (after === '1999') {
          return HttpResponse.json({
            namespace: 'default',
            streamType: 'task',
            streamID: params.id,
            afterSeq: 1999,
            latestSeq: 2500,
            events: [{
              id: 'default/task/my-task/2000',
              namespace: 'default',
              streamType: 'task',
              streamID: params.id,
              seq: 2000,
              type: 'ModelRequestCompleted',
              severity: 'info',
              createdAt: '2026-01-01T00:00:01Z',
            }],
          })
        }
        expect(after).toBe('2000')
        return HttpResponse.json({
          namespace: 'default',
          streamType: 'task',
          streamID: params.id,
          afterSeq: 2000,
          latestSeq: 2500,
          events: [],
        })
      }),
    )

    const { result } = renderHook(() => useTaskEvents('my-task'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(result.current.data?.latestSeq).toBe(2000)
    expect(result.current.data?.events.map((event) => event.seq)).toEqual([1999, 2000])

    await act(async () => {
      await result.current.refetch()
    })
    await waitFor(() => expect(result.current.data?.latestSeq).toBe(2500))
    expect(requests).toEqual([
      '?namespace=default&limit=1000',
      '?namespace=default&limit=1000&after=1999',
      '?namespace=default&limit=1000&after=2000',
    ])
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
