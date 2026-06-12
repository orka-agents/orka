import { describe, it, expect, vi } from 'vitest'
import { render, screen, within } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

const navigateMock = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    useNavigate: () => navigateMock,
  }
})

import { ExecutionGraph } from './execution-graph'
import type { Task } from '@/schemas/task'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    metadata: { name: 'parent-task', namespace: 'default', uid: 'uid-1' },
    spec: { type: 'agent', agentRef: { name: 'orchestrator' } },
    status: { phase: 'Running' },
    ...overrides,
  } as Task
}

describe('ExecutionGraph', () => {
  it('renders a tree (role=tree) with the root + each child as treeitems', () => {
    const task = makeTask({
      status: {
        phase: 'Running',
        childTasks: [
          { name: 'child-a', agent: 'reviewer', phase: 'Succeeded' },
          { name: 'child-b', agent: 'fixer', phase: 'Running' },
        ],
      },
    })
    render(<ExecutionGraph task={task} />)
    expect(screen.getByRole('tree', { name: /execution graph/i })).toBeInTheDocument()
    const items = screen.getAllByRole('treeitem')
    // root + 2 children
    expect(items).toHaveLength(3)
    expect(screen.getByText('parent-task')).toBeInTheDocument()
    expect(screen.getByText('child-a')).toBeInTheDocument()
    expect(screen.getByText('child-b')).toBeInTheDocument()
    expect(screen.getByText('reviewer')).toBeInTheDocument()
  })

  it('each node shows a phase dot sourced from the shared status module', () => {
    const task = makeTask({
      status: {
        phase: 'Running',
        childTasks: [{ name: 'child-a', agent: 'reviewer', phase: 'Succeeded' }],
      },
    })
    render(<ExecutionGraph task={task} />)
    const dots = screen.getAllByTestId('status-dot')
    expect(dots).toHaveLength(2)
    // Running root pulses; Succeeded child does not.
    const root = screen.getByRole('treeitem', { name: /parent-task \(Running\)/i })
    expect(within(root).getAllByTestId('status-dot')[0].className).toContain('bg-status-running')
  })

  it('running node carries the pulse class; terminal nodes do not', () => {
    const task = makeTask({
      status: {
        phase: 'Running',
        childTasks: [{ name: 'done-child', agent: 'x', phase: 'Succeeded' }],
      },
    })
    render(<ExecutionGraph task={task} />)
    const running = screen.getByRole('treeitem', { name: /parent-task \(Running\)/i })
    const done = screen.getByRole('treeitem', { name: /done-child \(Succeeded\)/i })
    expect(within(running).getAllByTestId('status-dot')[0].className).toContain('animate-pulse-live')
    expect(within(done).getAllByTestId('status-dot')[0].className).not.toContain('animate-pulse-live')
  })

  it('clicking a child node navigates to that task', async () => {
    navigateMock.mockClear()
    const task = makeTask({
      status: {
        phase: 'Running',
        childTasks: [{ name: 'child-a', agent: 'reviewer', phase: 'Succeeded' }],
      },
    })
    render(<ExecutionGraph task={task} />)
    await userEvent.click(screen.getByText('child-a'))
    expect(navigateMock).toHaveBeenCalledWith({
      to: '/tasks/$taskId',
      params: { taskId: 'child-a' },
    })
  })

  it('uses an onSelect override when provided', async () => {
    const onSelect = vi.fn()
    const task = makeTask({
      status: {
        phase: 'Running',
        childTasks: [{ name: 'child-a', agent: 'reviewer', phase: 'Succeeded' }],
      },
    })
    render(<ExecutionGraph task={task} onSelect={onSelect} />)
    await userEvent.click(screen.getByText('child-a'))
    expect(onSelect).toHaveBeenCalledWith('child-a')
  })

  it('degrades to a single root node when there are no children', () => {
    const task = makeTask({ status: { phase: 'Pending' } })
    render(<ExecutionGraph task={task} />)
    expect(screen.getAllByRole('treeitem')).toHaveLength(1)
    expect(screen.getByText('parent-task')).toBeInTheDocument()
  })

  it('nodes are keyboard-traversable and activate on Enter', async () => {
    navigateMock.mockClear()
    const task = makeTask({
      status: {
        phase: 'Running',
        childTasks: [{ name: 'child-a', agent: 'reviewer', phase: 'Succeeded' }],
      },
    })
    render(<ExecutionGraph task={task} />)
    const user = userEvent.setup()
    // Tab focuses the first node button; Tab again the child.
    await user.tab()
    await user.tab()
    expect(document.activeElement?.textContent).toContain('child-a')
    await user.keyboard('{Enter}')
    expect(navigateMock).toHaveBeenCalledWith({
      to: '/tasks/$taskId',
      params: { taskId: 'child-a' },
    })
  })

  it('renders the type icon for the root node', () => {
    const task = makeTask({ spec: { type: 'agent', agentRef: { name: 'o' } } })
    const { container } = render(<ExecutionGraph task={task} />)
    // root button carries an svg (type icon and/or dot)
    expect(container.querySelector('[role="treeitem"] svg')).toBeInTheDocument()
  })
})
