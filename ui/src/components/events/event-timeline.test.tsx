import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { EventTimeline } from './event-timeline'
import { makeEvent, resetEventSeq } from '@/test/fixtures/events'

const toastSuccess = vi.fn()
vi.mock('sonner', () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: vi.fn() } }))

describe('EventTimeline', () => {
  beforeEach(() => {
    resetEventSeq()
    toastSuccess.mockClear()
  })

  it('renders the empty state when there are no events', () => {
    render(<EventTimeline events={[]} emptyMessage="No execution events recorded." />)
    expect(screen.getByText('No execution events recorded.')).toBeInTheDocument()
  })

  it('renders a basic lifecycle timeline with seq, type, severity, and summary', () => {
    const events = [
      makeEvent({ seq: 1, type: 'TaskCreated', summary: 'task created' }),
      makeEvent({ seq: 2, type: 'TaskStarted', summary: 'task started' }),
      makeEvent({ seq: 3, type: 'TaskSucceeded', summary: 'task done' }),
    ]
    render(<EventTimeline events={events} />)
    const rows = screen.getAllByTestId('event-row')
    expect(rows).toHaveLength(3)
    expect(screen.getByText('TaskCreated')).toBeInTheDocument()
    expect(screen.getByText('task started')).toBeInTheDocument()
    expect(screen.getByText('#1')).toBeInTheDocument()
    // Lifecycle category label appears for lifecycle events.
    expect(screen.getAllByText('Lifecycle').length).toBeGreaterThan(0)
  })

  it('shows tool metadata for tool events', () => {
    const events = [
      makeEvent({ seq: 1, type: 'ToolCallStarted', toolName: 'web_fetch', toolCallID: 'call-12345678abcd', summary: 'fetching' }),
    ]
    render(<EventTimeline events={events} />)
    const row = screen.getByTestId('event-row')
    expect(within(row).getByText('Tools')).toBeInTheDocument()
    expect(within(row).getByText('tool: web_fetch')).toBeInTheDocument()
    // Tool call id is truncated for display (first 12 chars + ellipsis).
    expect(within(row).getByText(/call-1234567/)).toBeInTheDocument()
  })

  it('renders error severity with an accessible label', () => {
    const events = [makeEvent({ seq: 1, type: 'ToolCallFailed', severity: 'error', summary: 'boom' })]
    render(<EventTimeline events={events} />)
    const row = screen.getByTestId('event-row')
    expect(within(row).getByText('Error')).toBeInTheDocument() // sr-only severity label
  })

  it('shows a truncated marker for redacted/truncated payloads', () => {
    const events = [
      makeEvent({
        seq: 1,
        type: 'ModelMessage',
        summary: 'big message',
        truncation: { contentTextTruncated: true, contentTextOriginalChars: 9000 },
      }),
    ]
    render(<EventTimeline events={events} />)
    expect(screen.getByText('truncated')).toBeInTheDocument()
  })

  it('hides raw payload behind a disclosure toggle', async () => {
    const user = userEvent.setup()
    const events = [
      makeEvent({ seq: 1, type: 'ToolCallCompleted', content: { ok: true, value: 42 } }),
    ]
    render(<EventTimeline events={events} />)
    // Payload not shown by default.
    expect(screen.queryByTestId('event-payload')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /show payload/i }))
    expect(screen.getByTestId('event-payload')).toBeInTheDocument()
    expect(screen.getByTestId('event-payload').textContent).toContain('"value": 42')
  })

  it('filters by severity', async () => {
    const user = userEvent.setup()
    const events = [
      makeEvent({ seq: 1, type: 'TaskStarted', severity: 'info' }),
      makeEvent({ seq: 2, type: 'ToolCallFailed', severity: 'error', summary: 'failed call' }),
    ]
    render(<EventTimeline events={events} />)
    expect(screen.getAllByTestId('event-row')).toHaveLength(2)
    await user.selectOptions(screen.getByLabelText('Filter by severity'), 'error')
    const rows = screen.getAllByTestId('event-row')
    expect(rows).toHaveLength(1)
    expect(screen.getByText('failed call')).toBeInTheDocument()
  })

  it('searches summaries', async () => {
    const user = userEvent.setup()
    const events = [
      makeEvent({ seq: 1, type: 'TaskStarted', summary: 'alpha event' }),
      makeEvent({ seq: 2, type: 'ToolCallStarted', summary: 'beta event' }),
    ]
    render(<EventTimeline events={events} />)
    await user.type(screen.getByLabelText('Search events'), 'beta')
    expect(screen.getAllByTestId('event-row')).toHaveLength(1)
    expect(screen.getByText('beta event')).toBeInTheDocument()
  })

  it('shows a live indicator using the deep-ocean liveness token while streaming', () => {
    render(<EventTimeline events={[makeEvent({ seq: 1 })]} streamStatus="streaming" />)
    const live = screen.getByText('Live')
    expect(live).toBeInTheDocument()
    // Liveness is encoded with the reserved --color-live token, matching the
    // structured log viewer and live grid conventions from the redesign.
    expect(live.className).toContain('text-live')
  })

  it('shows stream complete when terminal', () => {
    render(<EventTimeline events={[makeEvent({ seq: 1 })]} streamStatus="complete" />)
    expect(screen.getByText('stream complete')).toBeInTheDocument()
  })

  it('shows an error with a retry button', async () => {
    const onRetry = vi.fn()
    const user = userEvent.setup()
    render(<EventTimeline events={[]} error="Failed to load events." onRetry={onRetry} />)
    expect(screen.getByText('Failed to load events.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /retry/i }))
    expect(onRetry).toHaveBeenCalled()
  })

  it('exposes a resume-from-seq helper for CLI/API users', () => {
    render(<EventTimeline events={[makeEvent({ seq: 7 })]} lastSeq={7} />)
    expect(screen.getByText(/after=7/)).toBeInTheDocument()
  })

  it('filters by category', async () => {
    const user = userEvent.setup()
    const events = [
      makeEvent({ seq: 1, type: 'TaskStarted', summary: 'lifecycle one' }),
      makeEvent({ seq: 2, type: 'ToolCallStarted', summary: 'a tool call' }),
    ]
    render(<EventTimeline events={events} />)
    expect(screen.getAllByTestId('event-row')).toHaveLength(2)
    await user.selectOptions(screen.getByLabelText('Filter by category'), 'tools')
    const rows = screen.getAllByTestId('event-row')
    expect(rows).toHaveLength(1)
    expect(screen.getByText('a tool call')).toBeInTheDocument()
  })

  it('copies the redacted API payload as JSON, not hidden raw data', async () => {
    const user = userEvent.setup()
    // userEvent installs a clipboard stub; spy on it after setup so the spy wins.
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined)
    render(
      <EventTimeline
        events={[makeEvent({ seq: 4, type: 'ToolCallCompleted', content: { ok: true }, summary: 's' })]}
      />,
    )
    await user.click(screen.getByRole('button', { name: 'Copy JSON' }))
    expect(writeText).toHaveBeenCalledTimes(1)
    const copied = writeText.mock.calls[0][0] as string
    // The copied text is the serialized event payload exactly as the API serves it.
    const parsed = JSON.parse(copied)
    expect(parsed.seq).toBe(4)
    expect(parsed.content).toEqual({ ok: true })
  })

  it('disclosure toggle is keyboard operable and wires aria-controls only while expanded', async () => {
    const user = userEvent.setup()
    render(<EventTimeline events={[makeEvent({ seq: 1, type: 'ToolCallCompleted', content: { x: 1 } })]} />)
    const toggle = screen.getByRole('button', { name: /show payload/i })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    // Collapsed: no dangling aria-controls IDREF (the target is not mounted yet).
    expect(toggle).not.toHaveAttribute('aria-controls')
    toggle.focus()
    expect(toggle).toHaveFocus()
    // Activate via keyboard (Enter), as a keyboard-only user would.
    await user.keyboard('{Enter}')
    const payload = screen.getByTestId('event-payload')
    expect(payload).toBeInTheDocument()
    const expandedToggle = screen.getByRole('button', { name: /hide payload/i })
    expect(expandedToggle).toHaveAttribute('aria-expanded', 'true')
    // Expanded: aria-controls now resolves to the mounted payload region.
    const controls = expandedToggle.getAttribute('aria-controls')
    expect(controls).toBeTruthy()
    expect(document.getElementById(controls!)).toContainElement(payload)
  })

  it('the event list and filters expose accessible labels', () => {
    render(<EventTimeline events={[makeEvent({ seq: 1 })]} />)
    expect(screen.getByRole('list', { name: 'Event timeline' })).toBeInTheDocument()
    expect(screen.getByLabelText('Search events')).toBeInTheDocument()
    expect(screen.getByLabelText('Filter by severity')).toBeInTheDocument()
    expect(screen.getByLabelText('Filter by category')).toBeInTheDocument()
  })
})
