import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { EventTimeline } from './event-timeline'
import { makeEvent, resetEventSeq } from '@/test/fixtures/events'

describe('EventTimeline', () => {
  beforeEach(() => resetEventSeq())

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

  it('shows a live indicator while streaming', () => {
    render(<EventTimeline events={[makeEvent({ seq: 1 })]} streamStatus="streaming" />)
    expect(screen.getByText('Live')).toBeInTheDocument()
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
})
