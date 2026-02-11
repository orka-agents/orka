import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => {
      const href = params?.taskId ? to.replace('$taskId', params.taskId) : to
      return <a href={href} {...props}>{children}</a>
    },
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/kanban' }),
  }
})

import { KanbanCard } from './kanban-card'
import type { Task } from '@/schemas/task'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    metadata: { name: 'test-task', namespace: 'default', uid: 'uid-1', creationTimestamp: new Date().toISOString() },
    spec: { type: 'container' },
    status: { phase: 'Pending' },
    ...overrides,
  }
}

describe('KanbanCard', () => {
  it('renders task name and type', () => {
    render(<KanbanCard task={makeTask()} />)
    expect(screen.getByText('test-task')).toBeInTheDocument()
    expect(screen.getByText('container')).toBeInTheDocument()
  })

  it('renders namespace', () => {
    render(<KanbanCard task={makeTask({ metadata: { name: 'ns-task', namespace: 'staging', uid: 'u2' } })} />)
    expect(screen.getByText('staging')).toBeInTheDocument()
  })

  it('navigation link points to task detail', () => {
    render(<KanbanCard task={makeTask()} />)
    const link = screen.getByText('test-task').closest('a')
    expect(link).toHaveAttribute('href', '/tasks/test-task')
  })

  it('shows child task count when present', () => {
    const task = makeTask({
      status: {
        phase: 'Running',
        childTasks: [
          { name: 'child-1', agent: 'a', phase: 'Running' },
          { name: 'child-2', agent: 'b', phase: 'Pending' },
        ],
      },
    })
    render(<KanbanCard task={task} />)
    expect(screen.getByTestId('child-count')).toHaveTextContent('2 children')
  })

  it('does not show child count when no children', () => {
    render(<KanbanCard task={makeTask()} />)
    expect(screen.queryByTestId('child-count')).not.toBeInTheDocument()
  })

  it('shows elapsed time for running tasks', () => {
    const task = makeTask({
      status: {
        phase: 'Running',
        startTime: new Date(Date.now() - 120_000).toISOString(),
      },
    })
    render(<KanbanCard task={task} />)
    expect(screen.getByTestId('elapsed-time')).toHaveTextContent('2m')
  })

  it('does not show elapsed time for non-running tasks', () => {
    render(<KanbanCard task={makeTask({ status: { phase: 'Succeeded' } })} />)
    expect(screen.queryByTestId('elapsed-time')).not.toBeInTheDocument()
  })

  it('shows agent name for agent tasks', () => {
    const task = makeTask({
      spec: { type: 'agent', agentRef: { name: 'code-reviewer' } },
    })
    render(<KanbanCard task={task} />)
    expect(screen.getByText('agent: code-reviewer')).toBeInTheDocument()
  })

  it('shows priority when set', () => {
    const task = makeTask({
      spec: { type: 'container', priority: 500 },
    })
    render(<KanbanCard task={task} />)
    expect(screen.getByText('pri: 500')).toBeInTheDocument()
  })

  it('does not show priority when zero', () => {
    const task = makeTask({
      spec: { type: 'container', priority: 0 },
    })
    render(<KanbanCard task={task} />)
    expect(screen.queryByText(/pri:/)).not.toBeInTheDocument()
  })
})
