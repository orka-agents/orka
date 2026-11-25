import { describe, expect, it, beforeEach, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen, waitFor, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
const { toastMock } = vi.hoisted(() => ({
  toastMock: Object.assign(vi.fn(), { success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))
vi.mock('sonner', () => ({ toast: toastMock }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { useOversightAlerts } from '@/stores/oversight-alerts'
import { OversightDashboard, type OversightSearch } from './oversight-dashboard'
import { charterConfig, charterDecision, makeDecision, riskyDecision, unavailableDecision } from '@/test/fixtures/oversight'
import type { DecisionView } from '@/schemas/oversight'

function serveList(items: DecisionView[], query?: { key: string; value: string }[]) {
  server.use(
    http.get('/api/v1/oversight/decisions', () => HttpResponse.json({ namespace: 'default', items, query })),
    http.get('/api/v1/oversight/decisions/:id', ({ params }) => {
      const found = items.find((d) => d.id === params.id)
      return found ? HttpResponse.json(found) : HttpResponse.json({ error: 'not found' }, { status: 404 })
    }),
  )
}

function renderDashboard(search: OversightSearch = {}) {
  const onSearchChange = vi.fn()
  render(<OversightDashboard search={search} onSearchChange={onSearchChange} />)
  return { onSearchChange }
}

describe('OversightDashboard', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    useOversightAlerts.getState().reset()
    toastMock.mockClear()
  })

  it('streams new decisions on top, resuming after the newest listed seq', async () => {
    let streamURL = ''
    serveList([makeDecision()])
    server.use(http.get('/api/v1/oversight/decisions/stream', ({ request }) => {
      streamURL = request.url
      const frame = `event: ready\ndata: {}\n\n: heartbeat\n\nid: 9\nevent: decision\ndata: ${JSON.stringify({ ...unavailableDecision, seq: 9 })}\n\n`
      return new HttpResponse(frame, { headers: { 'Content-Type': 'text/event-stream' } })
    }))
    renderDashboard()
    const table = await screen.findByRole('table', { name: 'Oversight decisions' })
    await waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(3))
    expect(new URL(streamURL).searchParams.get('after')).toBe('1')
    const rows = within(table).getAllByRole('row')
    expect(rows[1]).toHaveAttribute('data-signal', 'monitor_unavailable')
    expect(within(rows[1]).getByText('Scorer unavailable → would hold')).toBeInTheDocument()
  })

  it('pivots from the drawer to an all-time search', async () => {
    serveList([riskyDecision])
    const { onSearchChange } = renderDashboard({ window: '7d', decision: riskyDecision.id })
    const effects = await screen.findByRole('region', { name: 'Kernel-observed effects' })
    await userEvent.click(within(effects).getAllByRole('button', { name: 'Search all time for bin:wget' })[0])
    expect(onSearchChange).toHaveBeenLastCalledWith({ q: 'bin:wget', window: undefined, decision: undefined, view: undefined })
  })

  it('shows the echoed terms, the history panel and how many actions were searched', async () => {
    serveList([riskyDecision], [{ key: 'bin', value: 'wget' }])
    server.use(http.get('/api/v1/oversight/decisions/groups', ({ request }) => {
      const by = new URL(request.url).searchParams.get('by')
      return HttpResponse.json({
        namespace: 'default', by, searched: 120,
        groups: [{ key: by === 'task' ? 'build-1' : 'agent-a', count: 4, risky: 2, againstCharter: 1, firstSeen: '2026-10-01T00:00:00Z', lastSeen: '2026-11-25T00:00:00Z', tasks: ['build-1'] }],
      })
    }))
    const { onSearchChange } = renderDashboard({ q: 'bin:wget' })
    const panel = await screen.findByRole('region', { name: 'Has this happened before?' })
    expect(await within(panel).findByText(/Searched 120 actions across all time/)).toBeInTheDocument()
    expect(within(panel).getByText('agent-a')).toBeInTheDocument()
    await userEvent.click(within(panel).getByRole('button', { name: 'By task' }))
    expect(await within(panel).findByText('build-1')).toBeInTheDocument()
    const chips = screen.getByRole('list', { name: 'Search terms' })
    await userEvent.click(within(chips).getByRole('button', { name: 'Remove bin:wget' }))
    expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ q: undefined }))
  })

  it('surfaces a grammar refusal inline', async () => {
    server.use(http.get('/api/v1/oversight/decisions', () => HttpResponse.json(
      { error: { code: 400, message: 'invalid oversight query: feedback: takes one value' } }, { status: 400 })))
    renderDashboard({ q: 'feedback:a,b' })
    expect(await screen.findByText('invalid oversight query: feedback: takes one value')).toBeInTheDocument()
    expect(screen.getByLabelText('Search oversight decisions')).toHaveAttribute('aria-invalid', 'true')
  })

  it('stops on 501 with a clear state and never opens the stream', async () => {
    let listHits = 0
    let streamHits = 0
    server.use(
      http.get('/api/v1/oversight/decisions', () => { listHits++; return HttpResponse.json({ error: 'oversight is not enabled' }, { status: 501 }) }),
      http.get('/api/v1/oversight/decisions/stream', () => { streamHits++; return new HttpResponse('', { status: 501 }) }),
    )
    renderDashboard()
    expect(await screen.findAllByText('Oversight is not enabled')).not.toHaveLength(0)
    expect(listHits).toBe(1)
    expect(streamHits).toBe(0)
  })

  it('shows not authorized on 403', async () => {
    server.use(http.get('/api/v1/oversight/decisions', () => HttpResponse.json({ error: 'not authorized to read oversight decisions' }, { status: 403 })))
    renderDashboard()
    expect(await screen.findByText('Not authorized to view oversight decisions')).toBeInTheDocument()
  })

  it('bulk labels the selected results with one note', async () => {
    serveList([makeDecision(), riskyDecision])
    let body: unknown
    server.use(http.post('/api/v1/oversight/decisions/bulk-feedback', async ({ request }) => {
      body = await request.json()
      return HttpResponse.json({ recorded: 2, feedback: { label: 'real_problem', recordedAt: '2026-11-25T10:00:00Z' } })
    }))
    renderDashboard({ q: 'tool:code_exec' })
    await userEvent.click(await screen.findByRole('checkbox', { name: 'Select build-1@2' }))
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select build-1@1' }))
    const bar = screen.getByRole('region', { name: 'Selection actions' })
    expect(within(bar).getByText('2 selected')).toBeInTheDocument()
    await userEvent.type(within(bar).getByLabelText('Note for all selected (optional)'), 'confirmed')
    await userEvent.click(within(bar).getByRole('button', { name: 'Label real problem' }))
    await waitFor(() => expect(body).toEqual({ ids: ['build-1@2', 'build-1@1'], label: 'real_problem', note: 'confirmed' }))
  })

  it('drafts a concern from the search without writing anything', async () => {
    serveList([riskyDecision])
    let writes = 0
    server.use(http.post('*', () => { writes++; return HttpResponse.json({}) }))
    renderDashboard({ q: 'bin:wget' })
    await userEvent.click(await screen.findByRole('checkbox', { name: 'Select build-1@2' }))
    await userEvent.click(screen.getByRole('button', { name: /Draft concern from this search/ }))
    const yaml = (await screen.findByLabelText('Concern YAML draft')) as HTMLTextAreaElement
    expect(yaml.value).toContain('id: CONCERN-ID')
    expect(yaml.value).toContain('bin:wget')
    expect(yaml.value).toContain('Downloaded a model archive.')
    expect(writes).toBe(0)
  })

  it('switches to the question by action matrix', async () => {
    serveList([riskyDecision])
    const { onSearchChange } = renderDashboard()
    await screen.findByRole('table', { name: 'Oversight decisions' })
    await userEvent.click(screen.getByRole('tab', { name: 'Question × action' }))
    expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ view: 'matrix' }))
  })

  describe('charter alerts', () => {
    const streamOf = (...decisions: DecisionView[]) =>
      http.get('/api/v1/oversight/decisions/stream', () => new HttpResponse(
        `event: ready\ndata: {}\n\n${decisions.map((d) => `id: ${d.seq}\nevent: decision\ndata: ${JSON.stringify(d)}\n\n`).join('')}`,
        { headers: { 'Content-Type': 'text/event-stream' } },
      ))
    const streamedCharter = { ...charterDecision, id: 'build-9@1', seq: 20, taskName: 'build-9', agentName: 'agent-z' }

    beforeEach(() => {
      server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    })

    it('toasts and counts a streamed charter decision once, and opens it from the toast', async () => {
      serveList([makeDecision()])
      // The same decision twice (a replay) and a risky and an unavailable one, which never alert.
      server.use(streamOf(streamedCharter, { ...riskyDecision, seq: 21 }, { ...unavailableDecision, seq: 22 }, streamedCharter))
      const { onSearchChange } = renderDashboard()
      await waitFor(() => expect(toastMock).toHaveBeenCalledTimes(1))
      const [message, options] = toastMock.mock.calls[0]
      expect(message).toBe('Charter rule 2 broken by agent-z, would pause (audit mode)')
      expect(options.description).toBe('Never write outside the workspace.')
      expect(await screen.findByTestId('alerts-unread')).toHaveTextContent('1')
      options.action.onClick()
      expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ decision: 'build-9@1' }))
    })

    it('does not alert for charter decisions already in the initial page', async () => {
      serveList([charterDecision])
      server.use(streamOf({ ...riskyDecision, seq: 30 }))
      renderDashboard()
      await screen.findByText('Looks risky')
      expect(toastMock).not.toHaveBeenCalled()
      expect(screen.queryByTestId('alerts-unread')).not.toBeInTheDocument()
    })

    it('lists the alerts with links to the drawer and marks them read locally', async () => {
      serveList([makeDecision()])
      server.use(streamOf(streamedCharter))
      const { onSearchChange } = renderDashboard()
      await screen.findByTestId('alerts-unread')
      await userEvent.click(screen.getByRole('button', { name: 'Alerts, 1 unread' }))
      const list = await screen.findByRole('list', { name: 'Charter alerts' })
      expect(list).toHaveTextContent('Rule 2 of Workspace charter broken by agent-z, would pause (audit mode)')
      expect(list).not.toHaveTextContent(/blocked|stopped/i)
      await userEvent.click(screen.getByRole('button', { name: 'Mark all read' }))
      expect(screen.queryByTestId('alerts-unread')).not.toBeInTheDocument()
      await userEvent.click(within(list).getByRole('button', { name: 'Inspect build-9@1' }))
      expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ decision: 'build-9@1' }))
    })

    it('names the declared rule in the drawer', async () => {
      serveList([charterDecision])
      renderDashboard({ decision: charterDecision.id })
      expect(await screen.findByTestId('charter-rule')).toHaveTextContent('Rule 2 of Workspace charter')
    })
  })
})
