import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { RunTimeline } from './run-timeline'
import type { Task, PlanState } from '@/schemas/task'

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    metadata: {
      name: 'auto-task',
      namespace: 'default',
      uid: 'uid-1',
      creationTimestamp: '2026-01-01T00:00:00Z',
    },
    spec: { type: 'agent', agentRef: { name: 'looper' } },
    status: {
      phase: 'Running',
      iteration: 2,
      startTime: '2026-01-01T00:01:00Z',
    },
    ...overrides,
  } as Task
}

describe('RunTimeline', () => {
  it('renders events in chronological order from conditions + iteration', () => {
    const task = makeTask({
      status: {
        phase: 'Running',
        iteration: 1,
        startTime: '2026-01-01T00:01:00Z',
        conditions: [
          {
            type: 'PlanReady',
            status: 'True',
            lastTransitionTime: '2026-01-01T00:02:00Z',
          },
          {
            type: 'Scheduled',
            status: 'True',
            lastTransitionTime: '2026-01-01T00:00:30Z',
          },
        ],
      },
    })
    render(<RunTimeline task={task} plan={{ summary: 'do the thing' }} />)
    const labels = screen.getAllByRole('listitem').map((li) => li.textContent)
    // Created (00:00) → Scheduled (00:00:30) → Started (00:01) → PlanReady (00:02) → Iteration 1
    const text = labels.join('|')
    expect(text).toMatch(/Created/)
    const createdIdx = labels.findIndex((l) => l?.includes('Created'))
    const scheduledIdx = labels.findIndex((l) => l?.includes('Scheduled'))
    const startedIdx = labels.findIndex((l) => l?.includes('Started'))
    const planReadyIdx = labels.findIndex((l) => l?.includes('PlanReady'))
    expect(createdIdx).toBeLessThan(scheduledIdx)
    expect(scheduledIdx).toBeLessThan(startedIdx)
    expect(startedIdx).toBeLessThan(planReadyIdx)
  })

  it('shows the iteration with the plan summary as detail', () => {
    const task = makeTask()
    render(<RunTimeline task={task} plan={{ summary: 'refactor module' }} />)
    expect(screen.getByText('Iteration 2')).toBeInTheDocument()
    expect(screen.getByText('refactor module')).toBeInTheDocument()
  })

  it('renders a progress bar whose width reflects plan.progressPct', () => {
    const task = makeTask()
    const plan: PlanState = { summary: 's', progressPct: 42 }
    render(<RunTimeline task={task} plan={plan} />)
    const bar = screen.getByRole('progressbar', { name: /goal progress/i })
    expect(bar).toHaveAttribute('aria-valuenow', '42')
    const fill = bar.querySelector('div') as HTMLElement
    expect(fill.style.width).toBe('42%')
  })

  it('applies the completed treatment when goalComplete is true', () => {
    const task = makeTask()
    render(
      <RunTimeline
        task={task}
        plan={{ progressPct: 100, goalComplete: true }}
      />,
    )
    expect(screen.getByText('Goal complete')).toBeInTheDocument()
    const fill = screen
      .getByRole('progressbar')
      .querySelector('div') as HTMLElement
    expect(fill.className).toContain('bg-status-succeeded')
  })

  it('renders iteration tick marks for multi-iteration runs', () => {
    const task = makeTask({ status: { phase: 'Running', iteration: 3 } })
    render(<RunTimeline task={task} plan={{ progressPct: 60 }} />)
    const bar = screen.getByRole('progressbar')
    // iteration-1 tick marks => 2 ticks (the fill div + 2 ticks = 3 child divs/spans)
    const ticks = bar.querySelectorAll('span[aria-hidden="true"]')
    expect(ticks.length).toBe(2)
  })

  it('handles a missing plan gracefully (conditions-only timeline, no crash)', () => {
    const task = makeTask({
      status: {
        phase: 'Running',
        iteration: 1,
        conditions: [
          {
            type: 'Scheduled',
            status: 'True',
            lastTransitionTime: '2026-01-01T00:00:30Z',
          },
        ],
      },
    })
    render(<RunTimeline task={task} />)
    expect(screen.getByText('Scheduled')).toBeInTheDocument()
    expect(screen.getByText('Iteration 1')).toBeInTheDocument()
    // No progressbar when there is no progressPct.
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  })

  it('marks a terminal phase as a final done event', () => {
    const task = makeTask({
      status: {
        phase: 'Succeeded',
        iteration: 2,
        completionTime: '2026-01-01T01:00:00Z',
      },
    })
    render(<RunTimeline task={task} plan={{ summary: 's' }} />)
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
  })

  it('treats a Cancelled run as terminal (no pulsing iteration, shows a neutral terminal marker)', () => {
    const task = makeTask({
      status: {
        phase: 'Cancelled',
        iteration: 3,
        startTime: '2026-01-01T00:01:00Z',
        completionTime: '2026-01-01T00:30:00Z',
      },
    })
    const { container } = render(
      <RunTimeline task={task} plan={{ summary: 'stopped early' }} />,
    )
    const cancelled = screen.getByText('Cancelled')
    expect(cancelled).toBeInTheDocument()
    // Neutral (not failed/success) styling for a user-stopped run.
    expect(cancelled.className).toContain('text-muted-foreground')
    // The current iteration must NOT remain active/pulsing on a terminal run.
    expect(container.querySelector('[class*="animate-pulse-live"]')).toBeNull()
  })

  it('renders a Failed terminal event with failed styling (not success)', () => {
    const task = makeTask({
      status: {
        phase: 'Failed',
        iteration: 2,
        completionTime: '2026-01-01T01:00:00Z',
      },
    })
    render(<RunTimeline task={task} plan={{ summary: 's' }} />)
    const failedLabel = screen.getByText('Failed')
    // Label carries the failed token color, not success.
    expect(failedLabel.className).toContain('text-status-failed')
    expect(failedLabel.className).not.toContain('text-status-succeeded')
  })

  it('keeps the terminal event last even when it has a completionTime and the iteration does not', () => {
    // Regression: a timestamped terminal event must not sort ahead of the
    // undated synthetic "Iteration N" event.
    const task = makeTask({
      status: {
        phase: 'Succeeded',
        iteration: 4,
        startTime: '2026-01-01T00:01:00Z',
        completionTime: '2026-01-01T02:00:00Z',
        conditions: [
          {
            type: 'PlanReady',
            status: 'True',
            lastTransitionTime: '2026-01-01T00:02:00Z',
          },
        ],
      },
    })
    render(<RunTimeline task={task} plan={{ summary: 'done thinking' }} />)
    const labels = screen
      .getAllByRole('listitem')
      .map((li) => li.textContent ?? '')
    const iterationIdx = labels.findIndex((l) => l.includes('Iteration 4'))
    const terminalIdx = labels.findIndex((l) => l.includes('Succeeded'))
    expect(iterationIdx).toBeGreaterThanOrEqual(0)
    expect(terminalIdx).toBeGreaterThan(iterationIdx)
    // Terminal is the final event.
    expect(terminalIdx).toBe(labels.length - 1)
  })

  it('renders GenAI token/model telemetry from task events', () => {
    const task = makeTask()
    render(
      <RunTimeline
        task={task}
        events={[
          {
            id: 'event-1',
            namespace: 'default',
            streamType: 'task',
            streamID: 'auto-task',
            seq: 1,
            type: 'ModelRequestCompleted',
            severity: 'info',
            provider: 'anthropic',
            model: 'claude-sonnet-4',
            [`input${'Tokens'}`]: 12,
            [`output${'Tokens'}`]: 8,
            stopReason: 'end_turn',
            createdAt: '2026-01-01T00:02:00Z',
          },
        ]}
      />,
    )
    expect(screen.getByLabelText('GenAI telemetry')).toHaveTextContent(
      '20 tokens',
    )
    expect(screen.getByText(/claude-sonnet-4/)).toBeInTheDocument()
    expect(screen.getByText(/end_turn/)).toBeInTheDocument()
  })
})
