import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { RuntimeTimeline } from './runtime-timeline'
import type { ExecutionEvent } from '@/schemas/execution-event'

function ev(
  seq: number,
  type: string,
  summary: string,
  severity = 'info',
): ExecutionEvent {
  return {
    id: `e${seq}`,
    namespace: 'default',
    streamType: 'task',
    streamID: 't1',
    seq,
    type,
    severity,
    summary,
    createdAt: new Date(0).toISOString(),
  }
}

const events: ExecutionEvent[] = [
  ev(1, 'TaskCreated', 'task created'),
  ev(2, 'ModelRequestStarted', 'model thinking'),
  ev(3, 'ToolCallCompleted', 'ran web_search'),
  ev(4, 'TaskFailed', 'boom', 'error'),
]

describe('RuntimeTimeline', () => {
  it('renders empty state', () => {
    render(<RuntimeTimeline events={[]} />)
    expect(screen.getByText('No events')).toBeInTheDocument()
  })

  it('shows error events by default (not hidden)', () => {
    render(<RuntimeTimeline events={events} />)
    expect(screen.getByText('boom')).toBeInTheDocument()
    expect(screen.getByText('task created')).toBeInTheDocument()
  })

  it('filters to model-only when Model selected', async () => {
    const user = userEvent.setup()
    render(<RuntimeTimeline events={events} />)
    await user.click(screen.getByRole('button', { name: 'Model' }))
    expect(screen.getByText('model thinking')).toBeInTheDocument()
    expect(screen.queryByText('ran web_search')).not.toBeInTheDocument()
    expect(screen.queryByText('task created')).not.toBeInTheDocument()
  })

  it('keeps seq order and colors error severity', () => {
    render(<RuntimeTimeline events={events} />)
    const failed = screen.getByText('TaskFailed')
    expect(failed.className).toContain('text-status-failed')
  })

  it('shows unsupported message and hides rows', () => {
    render(<RuntimeTimeline events={events} status="unsupported" />)
    expect(screen.getByText('Live stream not enabled')).toBeInTheDocument()
    expect(screen.queryByText('task created')).not.toBeInTheDocument()
  })

  it('shows stream complete marker', () => {
    render(<RuntimeTimeline events={events} status="complete" />)
    expect(screen.getByText('— stream complete —')).toBeInTheDocument()
  })
})
