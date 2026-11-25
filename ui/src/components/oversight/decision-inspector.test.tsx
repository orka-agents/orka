import { describe, expect, it, beforeEach, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen, waitFor, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { DecisionInspector } from './decision-inspector'
import { charterConfig, charterDecision, makeDecision, producersResponse, riskyDecision } from '@/test/fixtures/oversight'
import type { DecisionView } from '@/schemas/oversight'

function serve(decision: DecisionView) {
  server.use(
    http.get('/api/v1/oversight/decisions/:id', ({ params }) =>
      params.id === decision.id ? HttpResponse.json(decision) : HttpResponse.json({ error: 'oversight decision not found' }, { status: 404 })),
    http.get('/api/v1/oversight/producers', () => HttpResponse.json(producersResponse)),
  )
}

function open(decision: DecisionView, props: Partial<React.ComponentProps<typeof DecisionInspector>> = {}) {
  const onPivot = vi.fn()
  const onOpenDecision = vi.fn()
  render(<DecisionInspector decisionId={decision.id} onClose={() => {}} onPivot={onPivot} onOpenDecision={onOpenDecision} {...props} />)
  return { onPivot, onOpenDecision }
}

/** Opens the collapsed Details, where the full record lives. */
async function openDetails() {
  await userEvent.click(await screen.findByRole('button', { name: 'Details' }))
}

describe('DecisionInspector', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('lists every scorer and advisor by producer with trust, status and scores', async () => {
    serve(riskyDecision)
    open(riskyDecision)
    await openDetails()
    const cards = await screen.findAllByTestId('scorer-card')
    expect(cards).toHaveLength(2)
    // Label from the producers endpoint, else the providerRef.
    expect(await within(cards[0]).findByText('Scorer Alpha')).toBeInTheDocument()
    expect(within(cards[1]).getByText('scorer-b')).toBeInTheDocument()
    expect(within(cards[0]).getByText('model-a')).toBeInTheDocument()
    expect(within(cards[0]).getByText(/sha256:aaaaaaaa/)).toBeInTheDocument()
    expect(within(cards[0]).getByText('qualified')).toBeInTheDocument()
    expect(within(cards[0]).getByText('(reported)')).toBeInTheDocument()
    expect(within(cards[0]).getByText('55 ms')).toBeInTheDocument()
    const k2 = within(cards[0]).getByRole('meter', { name: 'exfiltration probability' })
    expect(k2).toHaveAttribute('aria-valuenow', '0.91')
    expect(within(cards[0]).getByText(/0\.91 ≥ 0\.82 triggered/)).toBeInTheDocument()
    // A filtered scorer gets an explicit would-hold callout.
    expect(within(cards[1]).getByRole('alert')).toHaveTextContent('No usable answer (content_filtered) → would hold')
    expect(within(cards[1]).getByText('untrusted monitor')).toBeInTheDocument()
    const advisor = screen.getByTestId('advisor-card')
    expect(within(advisor).getByText('advisor-a')).toBeInTheDocument()
    expect(within(advisor).getByText('escalate')).toBeInTheDocument()
    expect(within(advisor).getByText('Looks like a download.')).toBeInTheDocument()
  })

  it('shows bytes sent only when the sensor observed sends', async () => {
    serve(riskyDecision)
    open(riskyDecision)
    await openDetails()
    expect(await screen.findByTestId('bytes-sent')).toHaveTextContent('2,048 bytes sent (kernel-observed)')
  })

  it('does not show bytes sent as zero when the sensor did not observe sends', async () => {
    const decision = makeDecision()
    serve(decision)
    open(decision)
    await openDetails()
    await screen.findByText('Kernel-observed effects')
    expect(screen.queryByTestId('bytes-sent')).not.toBeInTheDocument()
    expect(screen.queryByText(/bytes sent/)).not.toBeInTheDocument()
  })

  it('pivots a field to an all-time search', async () => {
    serve(riskyDecision)
    const { onPivot } = open(riskyDecision)
    await openDetails()
    const effects = await screen.findByRole('region', { name: 'Kernel-observed effects' })
    await userEvent.click(within(effects).getAllByRole('button', { name: 'Search all time for bin:wget' })[0])
    expect(onPivot).toHaveBeenLastCalledWith('bin:wget')
    await userEvent.click(within(effects).getAllByRole('button', { name: 'Search all time for dest:cdn.registry.example:443' })[0])
    expect(onPivot).toHaveBeenLastCalledWith('dest:cdn.registry.example:443')
    await userEvent.click(within(effects).getByRole('button', { name: 'Search all time for domain:registry.example' }))
    expect(onPivot).toHaveBeenLastCalledWith('domain:registry.example')
    await userEvent.click(within(effects).getByRole('button', { name: 'Search all time for path:/etc/*' }))
    expect(onPivot).toHaveBeenLastCalledWith('path:/etc/*')
    await userEvent.click(screen.getByRole('button', { name: 'Search all time for taint:untrusted_input' }))
    expect(onPivot).toHaveBeenLastCalledWith('taint:untrusted_input')
    await userEvent.click(screen.getByRole('button', { name: 'Search all time for concern:K10' }))
    expect(onPivot).toHaveBeenLastCalledWith('concern:K10')
    await userEvent.click(screen.getByRole('button', { name: /This agent's timeline/ }))
    expect(onPivot).toHaveBeenLastCalledWith('agent:agent-a')
  })

  it('highlights the cited notable event', async () => {
    serve(riskyDecision)
    open(riskyDecision)
    await openDetails()
    await userEvent.click(await screen.findByRole('button', { name: 'Highlight event ev-2' }))
    const table = screen.getByRole('table', { name: 'Notable events' })
    const rows = within(table).getAllByRole('row')
    expect(rows[2]).toHaveAttribute('data-highlighted', 'true')
    expect(rows[1]).not.toHaveAttribute('data-highlighted')
  })

  it('opens prior actions in the drawer', async () => {
    serve(riskyDecision)
    const { onOpenDecision } = open(riskyDecision)
    await openDetails()
    await userEvent.click(await screen.findByRole('button', { name: 'build-1@1' }))
    expect(onOpenDecision).toHaveBeenCalledWith('build-1@1')
  })

  it('records an outcome label with its note', async () => {
    serve(riskyDecision)
    let body: unknown
    server.use(http.post('/api/v1/oversight/decisions/:id/feedback', async ({ request, params }) => {
      body = { id: params.id, ...(await request.json() as object) }
      return HttpResponse.json({ recorded: 1, feedback: { label: 'false_alarm', note: 'benign', reviewer: 'alice', recordedAt: '2026-11-25T10:00:00Z' } })
    }))
    open(riskyDecision)
    await openDetails()
    await userEvent.type(await screen.findByLabelText('Label note (optional)'), 'benign')
    await userEvent.click(screen.getByRole('button', { name: 'False alarm' }))
    await waitFor(() => expect(body).toEqual({ id: 'build-1@2', label: 'false_alarm', note: 'benign' }))
    expect(await screen.findByText('alice')).toBeInTheDocument()
  })

  it('shows why similar actions matched', async () => {
    serve(riskyDecision)
    server.use(http.get('/api/v1/oversight/decisions/:id/similar', () => HttpResponse.json({
      namespace: 'default', source: riskyDecision, signature: [], searched: 40,
      items: [{
        decision: makeDecision({ id: 'other@1', taskName: 'other' }), score: 1, exactSignature: true,
        shared: [{ kind: 'tool', value: 'code_exec' }, { kind: 'dest_domain', value: 'registry.example' }, { kind: 'concern', value: 'K10' }],
      }],
    })))
    const { onPivot } = open(riskyDecision)
    await userEvent.click(await screen.findByRole('button', { name: /Has it done this before\?/ }))
    const item = await screen.findByTestId('similar-item')
    expect(within(item).getByText('100% alike')).toBeInTheDocument()
    expect(within(item).getByText('exact signature')).toBeInTheDocument()
    expect(within(item).getByText('same tool code_exec')).toBeInTheDocument()
    expect(within(item).getByText('dest_domain registry.example')).toBeInTheDocument()
    expect(within(item).getByText('concern K10')).toBeInTheDocument()
    expect(screen.getByText(/Compared 40 actions/)).toBeInTheDocument()
    await userEvent.click(within(item).getByRole('button', { name: 'Search all time for domain:registry.example' }))
    expect(onPivot).toHaveBeenCalledWith('domain:registry.example')
  })

  it('tells the story first and keeps the record under a collapsed Details', async () => {
    serve(riskyDecision)
    open(riskyDecision)
    expect(await screen.findByRole('heading', { name: 'sent 2 KB to cdn.registry.example:443 — would pause' })).toBeInTheDocument()
    expect(screen.getByTestId('strongest-evidence')).toHaveTextContent('kernel saw 2 KB sent to cdn.registry.example:443')
    expect(screen.queryByTestId('inspector-details')).not.toBeInTheDocument()
    expect(screen.queryByTestId('scorer-card')).not.toBeInTheDocument()
    const details = screen.getByRole('button', { name: 'Details' })
    expect(details).toHaveAttribute('aria-expanded', 'false')
    await userEvent.click(details)
    expect(details).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByTestId('inspector-details')).toBeInTheDocument()
  })

  it('says when no kernel evidence backs the story', async () => {
    const decision = makeDecision({ evidenceLevel: 'issued' })
    serve(decision)
    open(decision)
    expect(await screen.findByTestId('strongest-evidence')).toHaveTextContent('No kernel evidence: this is inferred from the agent\'s request (evidence issued).')
  })

  it('marks a false alarm from the story', async () => {
    serve(riskyDecision)
    let body: unknown
    server.use(http.post('/api/v1/oversight/decisions/:id/feedback', async ({ request }) => {
      body = await request.json()
      return HttpResponse.json({ recorded: 1, feedback: { label: 'false_alarm', reviewer: 'alice', recordedAt: '2026-11-25T10:00:00Z' } })
    }))
    open(riskyDecision)
    await userEvent.click(await screen.findByRole('button', { name: 'Mark false alarm' }))
    await waitFor(() => expect(body).toEqual({ label: 'false_alarm' }))
    expect(await screen.findByRole('button', { name: 'Marked false alarm' })).toBeDisabled()
  })

  it('shows the synthetic marker only in Details, labelled', async () => {
    const decision = makeDecision({ actionNarrative: '[synthetic] Task build-1 ran the test suite.', action: { toolName: 'code_exec', requestSummary: '[synthetic] run the tests', effects: {} } })
    serve(decision)
    open(decision)
    expect(await screen.findByRole('heading', { name: 'agent-a ran code_exec — run the tests' })).toBeInTheDocument()
    expect(screen.getByText('synthetic record')).toBeInTheDocument()
    await openDetails()
    expect(screen.getByText('[synthetic] Task build-1 ran the test suite.')).toBeInTheDocument()
    expect(screen.getByText(/marked this record synthetic/)).toBeInTheDocument()
  })

  it('lists the incident\'s actions when opened from an incident', async () => {
    serve(riskyDecision)
    const other = makeDecision({ id: 'build-1@9', seq: 9, wouldHavePaused: true, action: { toolName: 'code_exec', requestSummary: 'Second download', effects: {} } })
    const { onOpenDecision } = open(riskyDecision, { related: [riskyDecision, other] })
    const list = await screen.findByRole('list', { name: 'Actions in this incident' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    await userEvent.click(within(list).getByRole('button', { name: /Second download/ }))
    expect(onOpenDecision).toHaveBeenCalledWith('build-1@9')
  })

  it('shows concern names, with the code only as a muted label in Details', async () => {
    serve(riskyDecision)
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    open(riskyDecision)
    await openDetails()
    const cards = await screen.findAllByTestId('scorer-card')
    expect(await within(cards[0]).findByRole('button', { name: 'Search all time for concern:K2' })).toHaveTextContent('Data leaving the workspace')
    expect(within(cards[0]).getByText('K2')).toHaveClass('text-muted-foreground/80')
    const advisor = screen.getByTestId('advisor-card')
    expect(within(advisor).getByRole('button', { name: 'Search all time for concern:K10' })).toHaveTextContent('Unrequested download')
  })

  it('names rules by number, never by bound id, outside Details', async () => {
    serve(charterDecision)
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    open(charterDecision)
    expect(await screen.findByTestId('charter-rule')).toHaveTextContent('Rule 2 of Workspace charter')
    const story = screen.getByRole('region', { name: 'Story' })
    expect(story).not.toHaveTextContent('B1')
    await openDetails()
    const findings = screen.getByRole('region', { name: 'Charter findings' })
    expect(within(findings).getByRole('button', { name: 'Search all time for bound:B1' })).toHaveTextContent('Rule 2')
  })
})
