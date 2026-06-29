import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import {
  useTaskEvents,
  useSessionEvents,
  useTaskTrace,
  useTaskApprovals,
  useDecideApproval,
  useForkTask,
} from './use-execution-events'

const API = '/api/v1'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

describe('use-execution-events hooks', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
  })

  it('useTaskEvents requests events with after=0 and namespace', async () => {
    let capturedUrl = ''
    server.use(
      http.get(`${API}/tasks/:id/events`, ({ request, params }) => {
        capturedUrl = request.url
        return HttpResponse.json({
          namespace: 'default',
          streamType: 'task',
          streamID: params.id,
          afterSeq: 0,
          latestSeq: 2,
          events: [
            { id: 'e1', namespace: 'default', streamType: 'task', streamID: 'tk', seq: 1, type: 'TaskCreated', severity: 'info', createdAt: '2026-06-13T00:00:00Z' },
          ],
        })
      }),
    )
    const { result } = renderHook(() => useTaskEvents('tk'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    const url = new URL(capturedUrl)
    expect(url.pathname).toBe('/api/v1/tasks/tk/events')
    expect(url.searchParams.get('after')).toBe('0')
    expect(url.searchParams.get('namespace')).toBe('default')
    // Requests the server's max page size so completed tasks with many events
    // aren't truncated to the small default page.
    expect(url.searchParams.get('limit')).toBe('1000')
    expect(result.current.data?.latestSeq).toBe(2)
    expect(result.current.data?.events).toHaveLength(1)
  })

  it('useTaskEvents scopes its cache by task uid so a recreated same-name task does not inherit stale events', async () => {
    // Same name+namespace, different uid (a delete+recreate). Each uid must hit
    // the server and get its own cache entry — the recreated task must not show
    // the prior task's latestSeq/events.
    let calls = 0
    server.use(
      http.get(`${API}/tasks/:id/events`, () => {
        calls += 1
        // First fetch (uid-old) reports a high sequence; second fetch (uid-new)
        // reports the restarted low sequence. If the second hook were served from
        // the first's cache, it would never fetch and would show latestSeq 9.
        const latestSeq = calls === 1 ? 9 : 1
        return HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq, events: [],
        })
      }),
    )
    const wrapper = createWrapper()
    const first = renderHook(() => useTaskEvents('tk', true, 'uid-old'), { wrapper })
    await waitFor(() => expect(first.result.current.isSuccess).toBe(true))
    expect(first.result.current.data?.latestSeq).toBe(9)
    // A different uid under the same name must NOT return the cached uid-old entry —
    // it issues its own fetch and gets the recreated task's sequence.
    const second = renderHook(() => useTaskEvents('tk', true, 'uid-new'), { wrapper })
    await waitFor(() => expect(second.result.current.isSuccess).toBe(true))
    expect(second.result.current.data?.latestSeq).toBe(1)
    expect(calls).toBe(2)
  })

  it('useTaskEvents does not share a cache key with the full-history taskEvents hook', async () => {
    // The paged full-history hook in use-tasks.ts caches under ['taskEvents', ...].
    // This single-page replay hook must use a DISTINCT key so a refetch of one
    // never overwrites the other with an incompatible shape/partial page. Seed the
    // ['taskEvents', ...] entry with a sentinel and confirm this hook ignores it
    // and fetches its own data.
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
    })
    // Sentinel under the OTHER hook's key (same id/namespace/uid tuple).
    client.setQueryData(
      ['taskEvents', 'tk', 'default', 'uid-1'],
      { latestSeq: 999, events: [{ seq: 999 }] },
    )
    let fetched = false
    server.use(
      http.get(`${API}/tasks/:id/events`, () => {
        fetched = true
        return HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 3, events: [],
        })
      }),
    )
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    )
    const { result } = renderHook(() => useTaskEvents('tk', true, 'uid-1'), { wrapper })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    // It fetched fresh (didn't read the sentinel under the other hook's key) and
    // shows ITS own data, not the seeded 999 — proving the keys don't collide.
    expect(fetched).toBe(true)
    expect(result.current.data?.latestSeq).toBe(3)
  })

  it('useSessionEvents hits the session events endpoint', async () => {
    let capturedPath = ''
    server.use(
      http.get(`${API}/sessions/:id/events`, ({ request, params }) => {
        capturedPath = new URL(request.url).pathname
        return HttpResponse.json({
          namespace: 'default',
          streamType: 'session',
          streamID: params.id,
          afterSeq: 0,
          latestSeq: 0,
          events: [],
        })
      }),
    )
    const { result } = renderHook(() => useSessionEvents('sess'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(capturedPath).toBe('/api/v1/sessions/sess/events')
  })

  it('useTaskTrace polls when a refetch interval is provided', async () => {
    let calls = 0
    server.use(http.get('/api/v1/tasks/tk/trace', () => {
      calls += 1
      return HttpResponse.json({
        task: { namespace: 'default', name: 'tk', resultAvailable: false },
        latestSeq: calls, generatedAt: '2026-06-13T00:00:00Z', timeline: [],
        modelRequests: [], toolCalls: [], childTasks: [], workspace: [], artifacts: [], errors: [], warnings: [],
      })
    }))

    renderHook(() => useTaskTrace('tk', true, 'uid-trace', 20), { wrapper: createWrapper() })

    await waitFor(() => expect(calls).toBeGreaterThan(1))
  })

  it('useTaskTrace does not retry or poll when trace storage is unsupported', async () => {
    let calls = 0
    server.use(http.get('/api/v1/tasks/tk/trace', () => {
      calls += 1
      return HttpResponse.json({ error: 'not enabled' }, { status: 501 })
    }))

    const { result } = renderHook(() => useTaskTrace('tk', true, 'uid-unsupported', 20), { wrapper: createWrapper() })

    await waitFor(() => expect(result.current.error).toBeTruthy())
    await new Promise((resolve) => setTimeout(resolve, 80))
    expect(calls).toBe(1)
  })

  it('useTaskTrace fetches the trace payload', async () => {
    server.use(
      http.get(`${API}/tasks/:id/trace`, ({ params }) =>
        HttpResponse.json({
          task: { namespace: 'default', name: params.id, phase: 'Succeeded', resultAvailable: true },
          latestSeq: 5,
          generatedAt: '2026-06-13T00:00:00Z',
          timeline: [],
          modelRequests: [],
          toolCalls: [],
          childTasks: [],
          workspace: [],
          artifacts: [],
          errors: [],
          warnings: [],
        }),
      ),
    )
    const { result } = renderHook(() => useTaskTrace('tk'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.latestSeq).toBe(5)
    expect(result.current.data?.task.phase).toBe('Succeeded')
  })

  it('useTaskApprovals returns approvals list', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, ({ params }) =>
        HttpResponse.json({
          namespace: 'default',
          taskName: params.id,
          approvals: [
            { id: 'ap-1', action: 'web_fetch', status: 'pending', createdAt: '2026-06-13T00:00:00Z' },
          ],
        }),
      ),
    )
    const { result } = renderHook(() => useTaskApprovals('tk'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.approvals[0].id).toBe('ap-1')
  })

  it('useTaskApprovals stops polling once no approval is pending', async () => {
    let calls = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, ({ params }) => {
        calls += 1
        return HttpResponse.json({
          namespace: 'default',
          taskName: params.id,
          // All terminal — nothing pending, so polling must not continue.
          approvals: [{ id: 'ap-1', action: 'web_fetch', status: 'approved', createdAt: '2026-06-13T00:00:00Z' }],
        })
      }),
    )
    const { result } = renderHook(() => useTaskApprovals('tk', true, 50), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    const afterFirst = calls
    // Wait well beyond several poll intervals; with no pending approval the
    // refetchInterval function returns false, so no further requests fire.
    await new Promise((r) => setTimeout(r, 250))
    expect(calls).toBe(afterFirst)
  })

  it('useTaskApprovals keeps polling while the task is running even with no approvals', async () => {
    let calls = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, ({ params }) => {
        calls += 1
        // No approvals yet, but the task is still running so a future
        // ApprovalRequested could appear — polling must continue.
        return HttpResponse.json({ namespace: 'default', taskName: params.id, approvals: [] })
      }),
    )
    const { result } = renderHook(
      () => useTaskApprovals('tk', true, 50, /* taskRunning */ true),
      { wrapper: createWrapper() },
    )
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    await waitFor(() => expect(calls).toBeGreaterThan(1), { timeout: 1000 })
  })

  it('useTaskApprovals keeps polling while an approval is pending', async () => {
    let calls = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, ({ params }) => {
        calls += 1
        return HttpResponse.json({
          namespace: 'default',
          taskName: params.id,
          approvals: [{ id: 'ap-1', action: 'web_fetch', status: 'pending', createdAt: '2026-06-13T00:00:00Z' }],
        })
      }),
    )
    const { result } = renderHook(() => useTaskApprovals('tk', true, 50), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    // A pending approval keeps the poll alive, so more requests accrue over time.
    await waitFor(() => expect(calls).toBeGreaterThan(1), { timeout: 1000 })
  })

  it('useTaskApprovals stops polling once the task is terminal even with a pending approval', async () => {
    let calls = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, ({ params }) => {
        calls += 1
        // A pending approval on a terminal task (e.g. no expiry/cancel event was
        // ever written). It renders read-only and the backend rejects decisions,
        // so polling it would refetch the same row forever — it must stop.
        return HttpResponse.json({
          namespace: 'default',
          taskName: params.id,
          approvals: [{ id: 'ap-1', action: 'web_fetch', status: 'pending', createdAt: '2026-06-13T00:00:00Z' }],
        })
      }),
    )
    const { result } = renderHook(
      // pollIntervalMs set, not running, taskTerminal = true.
      () => useTaskApprovals('tk', true, 50, /* taskRunning */ false, /* taskTerminal */ true),
      { wrapper: createWrapper() },
    )
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    const afterFirst = calls
    // Wait well beyond several poll intervals; a terminal task must not keep
    // refetching its stuck-pending approval.
    await new Promise((r) => setTimeout(r, 250))
    expect(calls).toBe(afterFirst)
  })

  it('useDecideApproval posts the decision body', async () => {
    let capturedBody: unknown = null
    let capturedPath = ''
    server.use(
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, async ({ request, params }) => {
        capturedBody = await request.json()
        capturedPath = new URL(request.url).pathname
        return HttpResponse.json({
          id: params.approvalID,
          action: 'web_fetch',
          status: 'approved',
          createdAt: '2026-06-13T00:00:00Z',
        })
      }),
    )
    const { result } = renderHook(() => useDecideApproval('tk'), { wrapper: createWrapper() })
    await result.current.mutateAsync({ approvalId: 'ap-1', decision: 'approve', reason: 'ok' })
    expect(capturedPath).toBe('/api/v1/tasks/tk/approvals/ap-1/decision')
    expect(capturedBody).toEqual({ decision: 'approve', reason: 'ok' })
  })

  it('useForkTask posts afterSeq and returns the new task name', async () => {
    let capturedBody: unknown = null
    server.use(
      http.post(`${API}/tasks/:id/fork`, async ({ request, params }) => {
        capturedBody = await request.json()
        return HttpResponse.json(
          {
            namespace: 'default',
            sourceTaskName: params.id,
            newTaskName: 'tk-fork-1234',
            afterSeq: 3,
            forkContext: { sourceNamespace: 'default', sourceTask: params.id, afterSeq: 3, events: [], truncated: false },
          },
          { status: 201 },
        )
      }),
    )
    const { result } = renderHook(() => useForkTask('tk'), { wrapper: createWrapper() })
    const resp = await result.current.mutateAsync({ afterSeq: 3, newTaskName: 'tk-fork-1234' })
    expect(capturedBody).toEqual({ afterSeq: 3, newTaskName: 'tk-fork-1234' })
    expect(resp.newTaskName).toBe('tk-fork-1234')
    expect(resp.afterSeq).toBe(3)
  })

  it('useForkTask invalidates both event caches so full-history and replay views refresh', async () => {
    // The fork mutation must invalidate BOTH this hook's single-page replay key
    // (TASK_EVENTS_PAGE_KEY) AND the full-history ['taskEvents', ...] cache used by
    // the Overview/Execution panels (use-tasks.ts). A client.invalidateQueries spy
    // records which keys were invalidated.
    server.use(
      http.post(`${API}/tasks/:id/fork`, ({ params }) =>
        HttpResponse.json(
          {
            namespace: 'default', sourceTaskName: params.id, newTaskName: 'tk-fork-iv', afterSeq: 1,
            forkContext: { sourceNamespace: 'default', sourceTask: params.id, afterSeq: 1, events: [], truncated: false },
          },
          { status: 201 },
        ),
      ),
    )
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
    })
    const invalidated: unknown[] = []
    const spy = vi.spyOn(client, 'invalidateQueries').mockImplementation((filters?: { queryKey?: unknown }) => {
      invalidated.push(filters?.queryKey)
      return Promise.resolve()
    })
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    )
    const { result } = renderHook(() => useForkTask('tk'), { wrapper })
    await result.current.mutateAsync({ afterSeq: 1 })
    const keys = invalidated.map((k) => (Array.isArray(k) ? k[0] : k))
    // Both the single-page replay key and the full-history taskEvents key.
    expect(keys).toContain('taskEventsPage')
    expect(keys).toContain('taskEvents')
    spy.mockRestore()
  })

  it('useForkTask invalidates the trace query so fork provenance refreshes', async () => {
    // The Trace tab derives its Fork provenance section from the source task's
    // timeline; a successful fork must invalidate the taskTrace query so an
    // already-loaded trace refetches instead of going stale.
    let traceCalls = 0
    server.use(
      http.get(`${API}/tasks/:id/trace`, ({ params }) => {
        traceCalls += 1
        return HttpResponse.json({
          task: { namespace: 'default', name: params.id, resultAvailable: false },
          latestSeq: traceCalls, generatedAt: '2026-06-13T00:00:00Z',
          timeline: [], modelRequests: [], toolCalls: [], childTasks: [], workspace: [], artifacts: [], errors: [], warnings: [],
        })
      }),
      http.post(`${API}/tasks/:id/fork`, ({ params }) =>
        HttpResponse.json(
          {
            namespace: 'default', sourceTaskName: params.id, newTaskName: 'tk-fork-9', afterSeq: 2,
            forkContext: { sourceNamespace: 'default', sourceTask: params.id, afterSeq: 2, events: [], truncated: false },
          },
          { status: 201 },
        ),
      ),
    )
    // Both hooks share one QueryClient so the mutation's invalidation reaches the
    // trace query.
    const wrapper = createWrapper()
    const { result } = renderHook(
      () => ({ trace: useTaskTrace('tk'), fork: useForkTask('tk') }),
      { wrapper },
    )
    await waitFor(() => expect(result.current.trace.isSuccess).toBe(true))
    const before = traceCalls
    await result.current.fork.mutateAsync({ afterSeq: 2 })
    // The fork invalidates the trace, so it refetches.
    await waitFor(() => expect(traceCalls).toBeGreaterThan(before))
  })
})
