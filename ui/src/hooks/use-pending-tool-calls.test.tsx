import { describe, expect, it, beforeEach, vi } from 'vitest'
import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { usePendingToolCalls, type PendingToolCallsOptions } from './use-pending-tool-calls'

const task = (name: string, status: object = { phase: 'Running' }) => ({
  metadata: { name, namespace: 'default', uid: `uid-${name}`, creationTimestamp: '2026-11-25T10:00:00Z' },
  spec: { type: 'ai', agentRef: { name: 'agent-a' } },
  status,
})

const event = (taskName: string, seq: number, type = 'ToolProgress', extra: object = {}) => ({
  id: `${taskName}-${seq}`, namespace: 'default', streamType: 'task', streamID: taskName, seq, type, severity: 'info',
  taskName, createdAt: new Date().toISOString(), ...extra,
})

const toolCall = (taskName: string, seq: number, toolCallID: string, createdAt = new Date().toISOString()) =>
  event(taskName, seq, 'ToolCallCompleted', { toolName: 'code_exec', toolCallID, createdAt })

/** Serves events in pages of 200 after the cursor, with latestSeq the highest seq. */
function serveEvents(events: Record<string, object[]>) {
  server.use(http.get('/api/v1/tasks/:name/events', ({ params, request }) => {
    const after = Number(new URL(request.url).searchParams.get('after') ?? 0)
    const all = events[params.name as string] ?? []
    const list = all.filter((e) => (e as { seq: number }).seq > after).slice(0, 200)
    const latestSeq = Math.max(after, ...all.map((e) => (e as { seq: number }).seq))
    return HttpResponse.json({ namespace: 'default', streamType: 'task', streamID: params.name, afterSeq: after, latestSeq, events: list })
  }))
}

function renderPending(options: Partial<PendingToolCallsOptions> = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>
  return renderHook(() => usePendingToolCalls({ enabled: true, decisions: [], taskPollMs: 50, eventPollMs: 50, ...options }), { wrapper })
}

describe('usePendingToolCalls', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('follows the Task list past its first page', async () => {
    server.use(http.get('/api/v1/tasks', ({ request }) => {
      const next = new URL(request.url).searchParams.get('continue')
      return next
        ? HttpResponse.json({ items: [task('t2')], metadata: {} })
        : HttpResponse.json({ items: [task('t1', { phase: 'Succeeded', completionTime: '2026-01-01T00:00:00Z' })], metadata: { continue: 'page-2' } })
    }))
    serveEvents({ t2: [toolCall('t2', 1, 'c1')] })
    const { result, unmount } = renderPending()
    await waitFor(() => expect(result.current.pending.map((c) => c.toolCallID)).toEqual(['c1']))
    expect(result.current.candidates).toBe(1)
    expect(result.current.candidatesPartial).toBe(false)
    unmount()
  })

  it('drains a finished Task\'s events before it stops polling', async () => {
    const finished = task('t1', { phase: 'Succeeded', completionTime: new Date().toISOString() })
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({ items: [finished], metadata: {} })))
    const quiet = Array.from({ length: 200 }, (_, i) => event('t1', i + 1))
    serveEvents({ t1: [...quiet, toolCall('t1', 201, 'late')] })
    const { result, unmount } = renderPending()
    await waitFor(() => expect(result.current.pending.map((c) => c.toolCallID)).toEqual(['late']))
    unmount()
  })

  it('keeps expiring calls after the last Task ages out', async () => {
    let finished = false
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [finished ? task('t1', { phase: 'Succeeded', completionTime: '2026-01-01T00:00:00Z' }) : task('t1')], metadata: {},
    })))
    serveEvents({ t1: [toolCall('t1', 1, 'c1')] })
    const { result, unmount } = renderPending({ expiryMs: 400 })
    await waitFor(() => expect(result.current.pending).toHaveLength(1))
    finished = true
    await waitFor(() => expect(result.current.watching).toBe(0))
    await waitFor(() => expect(result.current.expired).toBe(1), { timeout: 3000 })
    expect(result.current.pending).toEqual([])
    unmount()
  })

  it('discards a response that lands after a namespace switch', async () => {
    let release: () => void = () => {}
    const gate = new Promise<void>((resolve) => { release = resolve })
    let requested = false
    let released = false
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const namespace = new URL(request.url).searchParams.get('namespace')
        return HttpResponse.json({ items: namespace === 'default' ? [task('t1')] : [task('t9')], metadata: {} })
      }),
      http.get('/api/v1/tasks/:name/events', async ({ params }) => {
        if (params.name === 't1') {
          requested = true
          await gate
          return HttpResponse.json({ namespace: 'default', streamType: 'task', streamID: 't1', afterSeq: 0, latestSeq: 1, events: [toolCall('t1', 1, 'stale')] })
        }
        // The new namespace's own call arrives later, which publishes the maps.
        const events = released ? [toolCall('t9', 1, 'fresh')] : []
        return HttpResponse.json({ namespace: 'other', streamType: 'task', streamID: params.name, afterSeq: 0, latestSeq: events.length, events })
      }),
    )
    const { result, unmount } = renderPending()
    await waitFor(() => expect(requested).toBe(true))
    act(() => useUIStore.setState({ namespace: 'other' }))
    await waitFor(() => expect(result.current.watching).toBe(1))
    release()
    await new Promise((r) => setTimeout(r, 100))
    released = true
    await waitFor(() => expect(result.current.pending.length).toBeGreaterThan(0))
    expect(result.current.pending.map((c) => c.toolCallID)).toEqual(['fresh'])
    unmount()
  })
})
