import { describe, expect, it, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('sonner', () => ({ toast: Object.assign(vi.fn(), { success: vi.fn(), error: vi.fn(), info: vi.fn() }) }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { useOversightAlerts } from '@/stores/oversight-alerts'
import { OversightDashboard, type OversightSearch } from './oversight-dashboard'
import { makeDecision } from '@/test/fixtures/oversight'

const encoder = new TextEncoder()

/** A live stream that stays open; push() sends a decision frame. */
function openStream() {
  let controller: ReadableStreamDefaultController<Uint8Array> | undefined
  server.use(http.get('/api/v1/oversight/decisions/stream', () => new HttpResponse(new ReadableStream<Uint8Array>({
    start(c) {
      controller = c
      c.enqueue(encoder.encode('event: ready\ndata: {}\n\n'))
    },
  }), { headers: { 'Content-Type': 'text/event-stream' } })))
  return {
    push: (decision: object & { seq: number }) =>
      controller?.enqueue(encoder.encode(`id: ${decision.seq}\nevent: decision\ndata: ${JSON.stringify(decision)}\n\n`)),
  }
}

const task = (name: string, created = '2026-11-25T10:00:00Z') => ({
  metadata: { name, namespace: 'default', uid: `uid-${name}`, creationTimestamp: created },
  spec: { type: 'ai', agentRef: { name: 'agent-a' } },
  status: { phase: 'Running' },
})

const toolCall = (taskName: string, toolCallID: string, toolName = 'code_exec', createdAt = new Date().toISOString(), seq = 1) => ({
  id: `${taskName}-${seq}`, namespace: 'default', streamType: 'task', streamID: taskName, seq, type: 'ToolCallCompleted', severity: 'info',
  taskName, toolName, toolCallID, createdAt,
})

function serveTasks(tasks: object[], events: Record<string, object[]>) {
  server.use(
    http.get('/api/v1/oversight/decisions', () => HttpResponse.json({ namespace: 'default', items: [] })),
    http.get('/api/v1/tasks', () => HttpResponse.json({ items: tasks, metadata: {} })),
    http.get('/api/v1/tasks/:name/events', ({ params, request }) => {
      const after = Number(new URL(request.url).searchParams.get('after') ?? 0)
      const list = (events[params.name as string] ?? []).filter((e) => (e as { seq: number }).seq > after)
      return HttpResponse.json({ namespace: 'default', streamType: 'task', streamID: params.name, afterSeq: after, latestSeq: after, events: list })
    }),
  )
}

function renderDashboard(search: OversightSearch) {
  render(<OversightDashboard search={search} onSearchChange={() => {}} />)
}

describe('seen, scoring… rows', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    useOversightAlerts.getState().reset()
  })

  it('shows a completed tool call as pending until its decision replaces it', async () => {
    const stream = openStream()
    serveTasks([task('t1')], { t1: [toolCall('t1', 'c1')] })
    renderDashboard({ view: 'activity' })
    const table = await screen.findByRole('table', { name: 'Oversight decisions' }, { timeout: 4000 })
    const row = await within(table).findByText('agent-a called code_exec · scoring…', {}, { timeout: 4000 })
    const tr = row.closest('tr')!
    expect(tr).toHaveAttribute('data-pending', 'true')
    expect(tr).toHaveAttribute('title', "Orka saw this tool call; the monitor's decision has not arrived yet")
    expect(within(tr).getByText('Seen · scoring…')).toBeInTheDocument()
    expect(within(tr).queryByRole('button')).not.toBeInTheDocument()
    expect(tr.querySelector('[data-badge]')).toBeNull()

    stream.push(makeDecision({ id: 't1@1', seq: 5, taskName: 't1', action: { toolName: 'code_exec', toolCallID: 'c1', requestSummary: 'run it', effects: {} } }))
    await waitFor(() => expect(within(table).queryByText('agent-a called code_exec · scoring…')).not.toBeInTheDocument())
    expect(within(table).getByRole('button', { name: 'agent-a ran code_exec — run it' })).toBeInTheDocument()
  })

  it('never shows pending rows in Needs you, only a count', async () => {
    openStream()
    serveTasks([task('t1')], { t1: [toolCall('t1', 'c1'), toolCall('t1', 'c2', 'shell', new Date().toISOString(), 2)] })
    renderDashboard({})
    expect(await screen.findByTestId('pending-note', {}, { timeout: 4000 })).toHaveTextContent('2 tool calls seen, awaiting decisions')
    expect(screen.queryByText(/scoring…/)).not.toBeInTheDocument()
  })

  it('drops a call with no decision after 10 minutes and counts it', async () => {
    openStream()
    const old = new Date(Date.now() - 11 * 60 * 1000).toISOString()
    serveTasks([task('t1')], { t1: [toolCall('t1', 'c1', 'code_exec', old)] })
    renderDashboard({ view: 'activity' })
    expect(await screen.findByTestId('pending-note', {}, { timeout: 4000 })).toHaveTextContent('1 with no decision after 10 min')
    expect(screen.queryByText(/scoring…/)).not.toBeInTheDocument()
  })

  it('says when the Task cap applies', async () => {
    openStream()
    const tasks = Array.from({ length: 31 }, (_, i) => task(`t${i}`, `2026-11-25T10:${String(i).padStart(2, '0')}:00Z`))
    serveTasks(tasks, {})
    renderDashboard({})
    expect(await screen.findByTestId('pending-note', {}, { timeout: 4000 })).toHaveTextContent('watching 30 of 31 running Tasks')
  })

  it('renders hostile event text as text', async () => {
    openStream()
    const hostile = '<img src=x onerror="window.__pwned=1">'
    serveTasks([task('t1')], { t1: [toolCall('t1', 'c1', hostile)] })
    renderDashboard({ view: 'activity' })
    expect(await screen.findByText(`agent-a called ${hostile} · scoring…`, {}, { timeout: 4000 })).toBeInTheDocument()
    expect(document.querySelector('img')).toBeNull()
  })

  it('stops asking for a Task\'s events after a 403', async () => {
    openStream()
    let hits = 0
    serveTasks([task('t1')], {})
    server.use(http.get('/api/v1/tasks/:name/events', () => { hits++; return HttpResponse.json({ error: 'no' }, { status: 403 }) }))
    renderDashboard({ view: 'activity' })
    await waitFor(() => expect(hits).toBe(1), { timeout: 4000 })
    await new Promise((r) => setTimeout(r, 2500))
    expect(hits).toBe(1)
  })
})
