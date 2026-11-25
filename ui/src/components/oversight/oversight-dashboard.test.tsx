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
import { charterConfig, charterDecision, makeDecision, makePosture, producersResponse, riskyDecision, unavailableDecision } from '@/test/fixtures/oversight'
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
    renderDashboard({ view: 'activity' })
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
    await userEvent.click(await screen.findByRole('button', { name: 'Details' }))
    const effects = await screen.findByRole('region', { name: 'Kernel-observed effects' })
    await userEvent.click(within(effects).getAllByRole('button', { name: 'Search all time for bin:wget' })[0])
    expect(onSearchChange).toHaveBeenLastCalledWith({ q: 'bin:wget', window: undefined, decision: undefined, view: 'activity' })
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
    await new Promise((r) => setTimeout(r, 100))
    // One request each for the feed and Needs you, never retried.
    expect(listHits).toBeLessThanOrEqual(2)
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
    const { onSearchChange } = renderDashboard({ view: 'activity' })
    await screen.findByRole('table', { name: 'Oversight decisions' })
    await userEvent.click(screen.getByRole('button', { name: 'Question × action' }))
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
      server.use(streamOf({ ...riskyDecision, id: 'build-1@30', seq: 30 }))
      renderDashboard({ view: 'activity' })
      const table = await screen.findByRole('table', { name: 'Oversight decisions' })
      await waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(3))
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

  describe('Needs you', () => {
    const pause = (n: number, agent = 'agent-a') => makeDecision({
      id: `${agent}@${n}`, seq: n, agentName: agent, wouldHavePaused: true, signal: 'risky', disposition: 'audit_flagged',
      actionNarrative: `[synthetic] pushed images to a forbidden path #${n}`, observedAt: `2026-11-25T10:0${n % 10}:00Z`,
    })

    it('is the default and groups what needs a person into incident cards', async () => {
      serveList([
        pause(1), pause(2), pause(3),
        { ...charterDecision, agentName: 'agent-b' },
        { ...unavailableDecision, agentName: 'agent-c' },
        makeDecision({ id: 'risky-only', seq: 50, signal: 'risky', disposition: 'audit_flagged' }),
        makeDecision({ id: 'quiet', seq: 51 }),
      ])
      server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
      renderDashboard()
      expect(screen.getByRole('tab', { name: /Needs you/ })).toHaveAttribute('data-state', 'active')
      const list = await screen.findByRole('list', { name: 'Incidents that need you' })
      await waitFor(() => expect(within(list).getAllByRole('button')).toHaveLength(3))
      const cards = within(list).getAllByRole('button')
      // Most severe first: charter break, would hold, would pause.
      expect(cards[0]).toHaveTextContent('agent-b ran code_exec — run the tests — breaks rule 2')
      expect(cards[0]).toHaveAttribute('title', 'Rule 2: Never write outside the workspace.')
      expect(cards[0].querySelectorAll('[data-badge]')).toHaveLength(1)
      expect(cards[1]).toHaveTextContent('Scorer unavailable → would hold')
      expect(cards[2]).toHaveTextContent('agent-a ran code_exec — run the tests — would pause')
      expect(cards[2]).not.toHaveTextContent('[synthetic]')
      expect(within(cards[2]).getByLabelText('3 actions')).toHaveTextContent('×3')
      expect(screen.queryByRole('table', { name: 'Oversight decisions' })).not.toBeInTheDocument()
    })

    it('asks the server for the attention-worthy signals in the posture window', async () => {
      const seen: string[] = []
      server.use(http.get('/api/v1/oversight/decisions', ({ request }) => {
        const url = new URL(request.url)
        seen.push(`${url.searchParams.get('q')}@${url.searchParams.get('window')}`)
        return HttpResponse.json({ namespace: 'default', items: [] })
      }))
      renderDashboard()
      expect(await screen.findByText('Nothing needs you')).toBeInTheDocument()
      expect(seen).toContain('signal:against_charter|monitor_unavailable|risky@24h')
    })

    it('caps the cards at the attention budget', async () => {
      serveList(Array.from({ length: 12 }, (_, i) => pause(i + 1, `agent-${i}`)))
      renderDashboard()
      const list = await screen.findByRole('list', { name: 'Incidents that need you' })
      await waitFor(() => expect(within(list).getAllByRole('listitem')).toHaveLength(10))
      await userEvent.click(screen.getByRole('button', { name: 'Show 2 more' }))
      expect(within(list).getAllByRole('listitem')).toHaveLength(12)
    })

    it('opens the newest decision of a card with the incident listed in the drawer', async () => {
      serveList([pause(1), pause(2)])
      const onSearchChange = vi.fn()
      const { rerender } = render(<OversightDashboard search={{}} onSearchChange={onSearchChange} />)
      const list = await screen.findByRole('list', { name: 'Incidents that need you' })
      await userEvent.click(within(list).getByRole('button'))
      expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ decision: 'agent-a@2' }))
      rerender(<OversightDashboard search={{ decision: 'agent-a@2' }} onSearchChange={onSearchChange} />)
      const actions = await screen.findByRole('list', { name: 'Actions in this incident' })
      expect(within(actions).getAllByRole('listitem')).toHaveLength(2)
    })

    it('shows posture as one line whose parts open their evidence', async () => {
      server.use(http.get('/api/v1/oversight/posture', () => HttpResponse.json(makePosture())))
      serveList([pause(1)])
      renderDashboard()
      const line = await screen.findByTestId('posture-line')
      await waitFor(() => expect(line).toHaveTextContent('Degraded · 1 incident · 80% seen · 1 blind spot · audit mode'))
      expect(line).not.toHaveTextContent(/warranted/i)
      expect(screen.getByTestId('posture-honesty')).toHaveTextContent('Audit mode: findings would pause an action; nothing is held.')
      expect(screen.getByTestId('posture-honesty')).toHaveTextContent('inferred')
      const blind = within(line).getByRole('button', { name: /1 blind spot/ })
      expect(blind).toHaveAttribute('aria-expanded', 'false')
      await userEvent.click(blind)
      expect(screen.getByRole('list', { name: 'Known blind spots' })).toHaveTextContent('dns_over_https')
      await userEvent.click(within(line).getByRole('button', { name: /80% seen/ }))
      expect(screen.getByText('8 of 10 actions attributed in the last 24h')).toBeInTheDocument()
      // The counters grid and canary basis are not on Needs you.
      expect(screen.queryByText('Canary basis')).not.toBeInTheDocument()
    })

    it('puts the cards first and the charter after them as one collapsed line', async () => {
      server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
      serveList([{ ...charterDecision, agentName: 'agent-b' }])
      renderDashboard()
      const toggle = await screen.findByRole('button', { name: /Demo charter · Rule 3 highlighted · 1 broken/ })
      expect(toggle).toHaveAttribute('aria-expanded', 'false')
      const list = await screen.findByRole('list', { name: 'Incidents that need you' })
      expect(list.compareDocumentPosition(toggle) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
      await userEvent.click(toggle)
      expect(screen.getByRole('list', { name: 'Rules of Workspace charter' })).toBeInTheDocument()
    })

    it('shows the funnel from kernel events to what needs you', async () => {
      server.use(http.get('/api/v1/oversight/posture', () => HttpResponse.json({
        ...makePosture(), basis: { ...makePosture().basis, actions: 1314, coverage: { ...makePosture().basis.coverage, observedEvents: 12408 } },
      })))
      serveList([pause(1)])
      renderDashboard()
      await waitFor(() => expect(screen.getByTestId('funnel-strip')).toHaveTextContent('12,408 kernel events → 1,314 actions → 1 need you'))
    })

    it('omits the kernel step when no events were observed, or from an older server', async () => {
      server.use(http.get('/api/v1/oversight/posture', () => HttpResponse.json(makePosture())))
      serveList([])
      renderDashboard()
      expect(await screen.findByTestId('funnel-strip')).toHaveTextContent(/^10 actions → 0 need you/)
    })
  })

  describe('charter view', () => {
    beforeEach(() => {
      server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    })

    it('opens from the posture line, which names the most-broken rule', async () => {
      serveList([{ ...charterDecision, agentName: 'agent-b' }, { ...charterDecision, id: 'build-2@2', seq: 4, agentName: 'agent-b' }])
      const { onSearchChange } = renderDashboard()
      const item = await screen.findByRole('button', { name: 'Charter: rule 2 broken ×2 · 1 of 3 rules broken' })
      await userEvent.click(item)
      expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ view: 'charter' }))
    })

    it('says no rule is broken when none is', async () => {
      serveList([])
      renderDashboard()
      expect(await screen.findByRole('button', { name: 'Charter: no rule broken · 0 of 3 rules broken' })).toBeInTheDocument()
    })

    it('lists every rule and opens the breaking actions in the same window', async () => {
      serveList([])
      server.use(http.get('/api/v1/oversight/decisions/groups', () => HttpResponse.json({ namespace: 'default', by: 'agent', groups: [], searched: 2 })))
      const { onSearchChange } = renderDashboard({ view: 'charter' })
      const rules = await screen.findByRole('list', { name: 'Rules of Workspace charter' })
      expect(within(rules).getAllByRole('listitem')).toHaveLength(3)
      expect(within(rules).getAllByRole('listitem')[0]).toHaveAttribute('data-highlight', 'true')
      await userEvent.click(await screen.findByRole('button', { name: 'Show the 2 actions that broke rule 1 in the last 24h' }))
      expect(onSearchChange).toHaveBeenLastCalledWith({ q: 'bound:B0', window: '24h', decision: undefined, view: 'activity' })
    })

    it('opens from a charter alert, in the toast and in the alerts list', async () => {
      serveList([makeDecision()])
      server.use(http.get('/api/v1/oversight/decisions/stream', () => new HttpResponse(
        `event: ready\ndata: {}\n\nid: 20\nevent: decision\ndata: ${JSON.stringify({ ...charterDecision, id: 'x@1', seq: 20 })}\n\n`,
        { headers: { 'Content-Type': 'text/event-stream' } },
      )))
      const { onSearchChange } = renderDashboard()
      await waitFor(() => expect(toastMock).toHaveBeenCalledTimes(1))
      toastMock.mock.calls[0][1].cancel.onClick()
      expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ view: 'charter' }))
      await userEvent.click(screen.getByRole('button', { name: 'Alerts, 1 unread' }))
      await userEvent.click(await screen.findByRole('button', { name: 'Rule 2' }))
      expect(onSearchChange).toHaveBeenLastCalledWith(expect.objectContaining({ view: 'charter' }))
    })
  })

  it('shows declared context as pills, and nothing when none is declared', async () => {
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    serveList([])
    const { unmount } = render(<OversightDashboard search={{}} onSearchChange={() => {}} />)
    const pills = await screen.findByRole('list', { name: 'Declared environment' })
    const pill = within(pills).getByText('Workload: synthetic fleet (drill)')
    expect(pill).toHaveAttribute('title', 'Declared by the operator, not verified by Orka')
    unmount()
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json({ queryKeys: [], maxBulkFeedback: 200 })))
    render(<OversightDashboard search={{}} onSearchChange={() => {}} />)
    await screen.findByTestId('posture-line')
    expect(screen.queryByRole('list', { name: 'Declared environment' })).not.toBeInTheDocument()
  })

  it('lists what the monitor asks on the Monitors tab', async () => {
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    serveList([])
    renderDashboard({ view: 'monitors' })
    const panel = await screen.findByRole('region', { name: 'What the monitor asks' })
    expect(await within(panel).findByText('Data leaving the workspace')).toBeInTheDocument()
    expect(within(panel).getByText(/Workspace data is sent somewhere the task did not ask for/)).toBeInTheDocument()
    expect(panel).not.toHaveTextContent('K2')
  })

  it('puts operator material on the Monitors tab', async () => {
    server.use(
      http.get('/api/v1/oversight/posture', () => HttpResponse.json(makePosture())),
      http.get('/api/v1/oversight/producers', () => HttpResponse.json(producersResponse)),
    )
    serveList([])
    renderDashboard({ view: 'monitors' })
    expect(await screen.findByText('Canary basis')).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Scorers' })).toBeInTheDocument()
    expect(await screen.findByText('(declared, not verified by Orka)')).toBeInTheDocument()
  })

  it('keeps the key grammar behind a help button', async () => {
    serveList([])
    renderDashboard({ view: 'activity' })
    const box = await screen.findByLabelText('Search oversight decisions')
    expect(box).toHaveAttribute('placeholder', 'try "push prod" or "token"')
    expect(screen.queryByText(/Narrow with key:value terms/)).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Search help' }))
    expect(screen.getByText(/Narrow with key:value terms/)).toBeInTheDocument()
  })
})
