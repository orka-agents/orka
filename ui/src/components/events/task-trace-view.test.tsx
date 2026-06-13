import { describe, it, expect, vi } from 'vitest'
import { render, screen, within } from '@/test/test-utils'
import { TaskTraceView } from './task-trace-view'
import { makeTrace } from '@/test/fixtures/trace'

// Render router Links as anchors so child/session links are assertable.
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => {
      let href = to
      if (typeof to === 'string' && params) {
        for (const [k, v] of Object.entries(params)) href = href.replace(`$${k}`, v as string)
      }
      return <a href={href} {...props}>{children}</a>
    },
  }
})

function traceEvent(seq: number, type: string, extra: Record<string, unknown> = {}) {
  return {
    seq,
    type,
    severity: 'info',
    createdAt: '2026-06-13T00:00:00.000Z',
    ...extra,
  }
}

describe('TaskTraceView', () => {
  it('renders a simple successful task trace with a lifecycle summary', () => {
    const trace = makeTrace({
      task: { namespace: 'default', name: 'tk', phase: 'Succeeded', type: 'agent', resultAvailable: true },
      latestSeq: 4,
      modelRequests: [{ id: 'm1', status: 'completed', startSeq: 1, endSeq: 2, summary: 'one call' }],
    })
    render(<TaskTraceView trace={trace} />)
    expect(screen.getByTestId('task-trace')).toBeInTheDocument()
    expect(screen.getByText('Lifecycle')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('result available')).toBeInTheDocument()
    expect(screen.getByText('latest #4')).toBeInTheDocument()
    expect(screen.getByText('Model requests')).toBeInTheDocument()
    expect(screen.getByText('one call')).toBeInTheDocument()
  })

  it('renders a tool-heavy trace', () => {
    const trace = makeTrace({
      toolCalls: [
        { id: 't1', name: 'web_fetch', status: 'completed', startSeq: 1, endSeq: 2 },
        { id: 't2', name: 'code_exec', status: 'completed', startSeq: 3, endSeq: 4 },
        { id: 't3', name: 'file_read', status: 'running', startSeq: 5 },
      ],
    })
    render(<TaskTraceView trace={trace} />)
    const section = screen.getByText('Tool calls').closest('section')!
    expect(within(section).getByText('web_fetch')).toBeInTheDocument()
    expect(within(section).getByText('code_exec')).toBeInTheDocument()
    expect(within(section).getByText('file_read')).toBeInTheDocument()
  })

  it('renders a failed trace with errors prominently', () => {
    const trace = makeTrace({
      task: { namespace: 'default', name: 'tk', phase: 'Failed', resultAvailable: false },
      toolCalls: [{ id: 't1', name: 'web_fetch', status: 'failed', startSeq: 1, endSeq: 2, error: 'timeout' }],
      errors: [{ seq: 2, type: 'ToolCallFailed', severity: 'error', message: 'web_fetch timed out' }],
    })
    render(<TaskTraceView trace={trace} />)
    const errorsSection = screen.getByText('Errors').closest('section')!
    expect(within(errorsSection).getByText('web_fetch timed out')).toBeInTheDocument()
    // Phase shows Failed in the lifecycle summary.
    expect(screen.getByText('Failed')).toBeInTheDocument()
  })

  it('renders child tasks as links to their detail pages', () => {
    const trace = makeTrace({
      childTasks: [
        { name: 'child-1', agent: 'planner', status: 'succeeded', startSeq: 1, endSeq: 2 },
        { name: 'child-2', agent: 'coder', status: 'running', startSeq: 3 },
      ],
    })
    render(<TaskTraceView trace={trace} />)
    const section = screen.getByText('Child tasks').closest('section')!
    const link1 = within(section).getByRole('link', { name: 'child-1' })
    expect(link1).toHaveAttribute('href', '/tasks/child-1')
    expect(within(section).getByRole('link', { name: 'child-2' })).toBeInTheDocument()
  })

  it('surfaces approval events from the timeline', () => {
    const trace = makeTrace({
      latestSeq: 2,
      timeline: [
        traceEvent(1, 'TaskStarted', { summary: 'started' }),
        traceEvent(2, 'ApprovalRequested', { summary: 'approve web_fetch?', toolName: 'web_fetch' }),
      ],
      modelRequests: [{ id: 'm1', status: 'completed', startSeq: 1 }],
    })
    render(<TaskTraceView trace={trace} />)
    // "Approvals" also appears as a category badge inside the row, so scope to
    // the section by its heading.
    const section = screen.getByRole('heading', { name: 'Approvals' }).closest('section')!
    expect(within(section).getByText('ApprovalRequested')).toBeInTheDocument()
    expect(within(section).getByText('approve web_fetch?')).toBeInTheDocument()
  })

  it('surfaces fork provenance from the timeline', () => {
    const trace = makeTrace({
      latestSeq: 3,
      timeline: [
        traceEvent(1, 'TaskStarted'),
        traceEvent(3, 'TaskForkCreated', { summary: 'task fork created' }),
      ],
      toolCalls: [{ id: 't1', name: 'web_fetch', status: 'completed', startSeq: 2 }],
    })
    render(<TaskTraceView trace={trace} />)
    // "Fork" appears as a category badge inside the row, so scope by heading.
    const section = screen.getByRole('heading', { name: 'Fork provenance' }).closest('section')!
    expect(within(section).getByText('TaskForkCreated')).toBeInTheDocument()
  })

  it('falls back to the raw timeline when there are no structured groups', () => {
    const trace = makeTrace({
      latestSeq: 2,
      timeline: [
        traceEvent(1, 'TaskCreated', { summary: 'created' }),
        traceEvent(2, 'TaskPhaseChanged', { summary: 'pending → running' }),
      ],
    })
    render(<TaskTraceView trace={trace} />)
    const section = screen.getByText('Raw timeline').closest('section')!
    expect(within(section).getByText('TaskCreated')).toBeInTheDocument()
    expect(within(section).getByText('pending → running')).toBeInTheDocument()
  })

  it('renders an empty state when there is nothing to show', () => {
    render(<TaskTraceView trace={makeTrace()} />)
    expect(screen.getByText(/no execution events recorded/i)).toBeInTheDocument()
  })
})
