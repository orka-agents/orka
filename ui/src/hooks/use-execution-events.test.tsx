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
})
