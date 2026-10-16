import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { ActivitySpotlight } from './activity-spotlight'
import type { Task } from '@/schemas/task'
import type { ExecutionEvent } from '@/schemas/execution-event'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to }: any) => <a href={to}>{children}</a> }
})

function task(name: string, o: Partial<Task> = {}): Task {
  return {
    metadata: { name, namespace: 'default', uid: name, ...o.metadata },
    spec: { type: 'agent', ...o.spec },
    status: o.status ?? { phase: 'Running', startTime: new Date(0).toISOString() },
  }
}

describe('ActivitySpotlight', () => {
  it('renders idle empty state when no task', () => {
    render(<ActivitySpotlight task={null} following={false} />)
    expect(screen.getByText('No active task')).toBeInTheDocument()
  })

  it('shows active task name, agent, and following state', () => {
    render(<ActivitySpotlight task={task('r1', { spec: { type: 'agent', agentRef: { name: 'alpha' } } })} following />)
    expect(screen.getByText('r1')).toBeInTheDocument()
    expect(screen.getByText('alpha')).toBeInTheDocument()
    expect(screen.getByText('Following live')).toBeInTheDocument()
  })

  it('keeps a long agent name on one line and wraps the row instead', () => {
    const agent = 'repository-security-analysis-assistant'
    render(
      <ActivitySpotlight
        task={task('a1', {
          spec: { type: 'agent', agentRef: { name: agent } },
          status: { phase: 'Running', startTime: new Date(0).toISOString(), conditions: [{ type: 'WaitingForApproval', status: 'True' }] },
        })}
        following
      />,
    )
    const name = screen.getByText(agent)
    expect(name).toHaveAttribute('title', agent)
    expect(name).toHaveClass('truncate', 'min-w-0', 'max-w-full')
    expect(name.parentElement).toHaveClass('flex', 'flex-wrap')
    expect(name.parentElement).toContainElement(screen.getByText('Waiting for approval'))
  })

  it('falls back to unassigned when no agentRef and shows paused', () => {
    render(<ActivitySpotlight task={task('r2')} following={false} />)
    expect(screen.getByText('unassigned')).toBeInTheDocument()
    expect(screen.getByText('Paused')).toBeInTheDocument()
  })

  it('shows the waiting status message instead of the last event while parked on an approval', () => {
    const ev = { summary: 'AI worker completed' } as ExecutionEvent
    render(
      <ActivitySpotlight
        task={task('w1', { status: { phase: 'Running', startTime: new Date(0).toISOString(), message: 'waiting for approval ap-7 for bash at iteration 1' } })}
        latestEvent={ev}
        following
      />,
    )
    expect(screen.getByText('waiting for approval ap-7 for bash at iteration 1')).toBeInTheDocument()
    expect(screen.queryByText('AI worker completed')).not.toBeInTheDocument()
    expect(screen.getByText('Waiting for approval')).toBeInTheDocument()
  })

  it('surfaces a stuck Pod as a warning instead of the last event', () => {
    const ev = { summary: 'Task started with Job pull-1-abc' } as ExecutionEvent
    const message = 'pod waiting: ImagePullBackOff - Back-off pulling image "ghcr.io/example/missing:v1"'
    render(
      <ActivitySpotlight
        task={task('pull-1', { status: { phase: 'Running', startTime: new Date(0).toISOString(), message } })}
        latestEvent={ev}
        following
      />,
    )
    const warning = screen.getByRole('status')
    expect(warning).toHaveTextContent(message)
    expect(warning).toHaveClass('text-status-pending')
    expect(screen.queryByText('Task started with Job pull-1-abc')).not.toBeInTheDocument()
  })

  it('prefers latest event summary over a running status message', () => {
    const ev = { summary: 'tool call completed' } as ExecutionEvent
    render(<ActivitySpotlight task={task('r-status', { status: { phase: 'Running', message: 'generic running status' } })} latestEvent={ev} following />)
    expect(screen.getByText('tool call completed')).toBeInTheDocument()
    expect(screen.queryByText('generic running status')).not.toBeInTheDocument()
  })

  it('shows latest event summary when no status message', () => {
    const ev = { summary: 'tool call completed' } as ExecutionEvent
    render(<ActivitySpotlight task={task('r3')} latestEvent={ev} following />)
    expect(screen.getByText('tool call completed')).toBeInTheDocument()
  })

  it('renders dash elapsed for running task with no startTime', () => {
    render(<ActivitySpotlight task={task('r4', { status: { phase: 'Running' } })} following />)
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('labels a pending task as waiting, not active', () => {
    render(<ActivitySpotlight task={task('queued', { status: { phase: 'Pending' } })} following />)
    expect(screen.getByText('Waiting')).toBeInTheDocument()
    expect(screen.getByText('Not running')).toBeInTheDocument()
    expect(screen.queryByText('Active now')).not.toBeInTheDocument()
    expect(screen.queryByText('Following live')).not.toBeInTheDocument()
  })

  it('labels a terminal task as completed, not active', () => {
    render(<ActivitySpotlight task={task('done', { status: { phase: 'Succeeded', startTime: new Date(0).toISOString(), completionTime: new Date(5000).toISOString() } })} following={false} />)
    expect(screen.getByText('Last run')).toBeInTheDocument()
    expect(screen.getByText('Completed')).toBeInTheDocument()
    expect(screen.queryByText('Active now')).not.toBeInTheDocument()
    expect(screen.getByText('5s')).toBeInTheDocument() // frozen at completion, not growing
  })
})
