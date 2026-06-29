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

  it('falls back to unassigned when no agentRef and shows paused', () => {
    render(<ActivitySpotlight task={task('r2')} following={false} />)
    expect(screen.getByText('unassigned')).toBeInTheDocument()
    expect(screen.getByText('Paused')).toBeInTheDocument()
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

  it('labels a terminal task as completed, not active', () => {
    render(<ActivitySpotlight task={task('done', { status: { phase: 'Succeeded', startTime: new Date(0).toISOString(), completionTime: new Date(5000).toISOString() } })} following={false} />)
    expect(screen.getByText('Last run')).toBeInTheDocument()
    expect(screen.getByText('Completed')).toBeInTheDocument()
    expect(screen.queryByText('Active now')).not.toBeInTheDocument()
    expect(screen.getByText('5s')).toBeInTheDocument() // frozen at completion, not growing
  })
})
