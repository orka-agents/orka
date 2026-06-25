import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => (
      <a href={to} {...props}>
        {children}
      </a>
    ),
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/tasks' }),
  }
})

import { TaskExecutionPanel } from './task-execution-panel'
import type { Task } from '@/schemas/task'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    metadata: { name: 'test-task', namespace: 'default', uid: 'uid-1' },
    spec: { type: 'container', image: 'alpine' },
    status: { phase: 'Running', attempts: 1 },
    ...overrides,
  }
}

describe('TaskExecutionPanel', () => {
  it('renders with Pending phase', () => {
    render(
      <TaskExecutionPanel
        task={makeTask({ status: { phase: 'Pending', attempts: 0 } })}
      />,
    )
    expect(screen.getAllByText('Pending').length).toBeGreaterThanOrEqual(1)
    expect(screen.getByText('Execution')).toBeInTheDocument()
    expect(screen.getByTestId('progress-steps')).toBeInTheDocument()
  })

  it('treats a Scheduled task as not-yet-started (Completed step is not current)', () => {
    const { container } = render(
      <TaskExecutionPanel
        task={makeTask({ status: { phase: 'Scheduled', attempts: 0 } })}
      />,
    )
    const steps = container.querySelector(
      '[data-testid="progress-steps"]',
    ) as HTMLElement
    const circles = steps.querySelectorAll('.rounded-full')
    expect(circles).toHaveLength(3) // Pending / Running / Completed
    // Pending (step 0) is the current/highlighted step.
    expect(circles[0].className).toContain('bg-status-running-bg')
    // Completed (step 2) is NOT highlighted and still shows its index ("3") —
    // a scheduled task is not mislabeled as completed.
    expect(circles[2].className).toContain('bg-muted')
    expect(circles[2].textContent).toBe('3')
  })

  it('renders with Running phase', () => {
    render(
      <TaskExecutionPanel
        task={makeTask({
          status: {
            phase: 'Running',
            attempts: 1,
            startTime: new Date().toISOString(),
          },
        })}
      />,
    )
    expect(screen.getAllByText('Running').length).toBeGreaterThanOrEqual(1)
    const elapsed = screen.getByTestId('elapsed-time')
    expect(elapsed.textContent).toMatch(/\d+s/)
  })

  it('renders with Succeeded phase', () => {
    const start = new Date(Date.now() - 120000).toISOString()
    const end = new Date().toISOString()
    render(
      <TaskExecutionPanel
        task={makeTask({
          status: {
            phase: 'Succeeded',
            attempts: 2,
            startTime: start,
            completionTime: end,
          },
        })}
      />,
    )
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    const elapsed = screen.getByTestId('elapsed-time')
    expect(elapsed.textContent).toMatch(/2m/)
  })

  it('renders with Failed phase and message', () => {
    render(
      <TaskExecutionPanel
        task={makeTask({
          status: { phase: 'Failed', attempts: 3, message: 'OOMKilled' },
        })}
      />,
    )
    expect(screen.getByText('Failed')).toBeInTheDocument()
    expect(screen.getByText('OOMKilled')).toBeInTheDocument()
    // Attempts count "3" appears both in step indicator and attempts display
    expect(screen.getAllByText('3').length).toBeGreaterThanOrEqual(1)
  })

  it('shows elapsed as dash when no startTime', () => {
    render(
      <TaskExecutionPanel
        task={makeTask({ status: { phase: 'Pending', attempts: 0 } })}
      />,
    )
    const elapsed = screen.getByTestId('elapsed-time')
    expect(elapsed.textContent).toBe('-')
  })

  it('displays child task summary', () => {
    render(
      <TaskExecutionPanel
        task={makeTask({
          status: {
            phase: 'Running',
            attempts: 1,
            childTasks: [
              { name: 'child-1', agent: 'agent-a', phase: 'Running' },
              { name: 'child-2', agent: 'agent-b', phase: 'Succeeded' },
              { name: 'child-3', agent: 'agent-a', phase: 'Failed' },
              { name: 'child-4', agent: 'agent-c', phase: 'Pending' },
            ],
          },
        })}
      />,
    )
    const summary = screen.getByTestId('child-task-summary')
    expect(summary).toBeInTheDocument()
    expect(summary.textContent).toContain('4 total')
    expect(summary.textContent).toContain('1 running')
    expect(summary.textContent).toContain('1 succeeded')
    expect(summary.textContent).toContain('1 failed')
    expect(summary.textContent).toContain('1 pending')
  })

  it('displays GenAI token rollup from execution events', () => {
    render(
      <TaskExecutionPanel
        task={makeTask({ status: { phase: 'Succeeded', attempts: 1 } })}
        events={[
          {
            id: 'event-1',
            namespace: 'default',
            streamType: 'task',
            streamID: 'test-task',
            seq: 1,
            type: 'ModelRequestCompleted',
            severity: 'info',
            provider: 'openai',
            model: 'gpt-4o',
            inputTokens: 100,
            outputTokens: 25,
            stopReason: 'stop',
            createdAt: new Date().toISOString(),
          },
        ]}
      />,
    )
    const rollup = screen.getByLabelText('GenAI token rollup')
    expect(rollup).toHaveTextContent('125 total')
    expect(rollup).toHaveTextContent('gpt-4o')
    expect(rollup).toHaveTextContent('openai')
  })
})
