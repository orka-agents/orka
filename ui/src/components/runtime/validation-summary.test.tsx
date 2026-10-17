import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { ValidationSummary } from './validation-summary'
import type { Task, TaskPhase } from '@/schemas/task'
import type { TaskTrace, Approval } from '@/schemas/execution-event'

function task(phase: TaskPhase, extra: Record<string, unknown> = {}): Task {
  return {
    metadata: { name: 't1', namespace: 'default', uid: 't1' },
    spec: { type: 'agent', agentRef: { name: 'alpha' } },
    status: { phase, ...extra },
  }
}

function trace(over: Partial<TaskTrace> = {}): TaskTrace {
  return {
    task: { namespace: 'default', name: 't1', resultAvailable: true },
    latestSeq: 1,
    generatedAt: new Date().toISOString(),
    timeline: [],
    modelRequests: [],
    toolCalls: [],
    childTasks: [],
    workspace: [],
    artifacts: [],
    errors: [],
    warnings: [],
    ...over,
  }
}

describe('ValidationSummary', () => {
  it('always labels itself as not a formal evaluation', () => {
    render(<ValidationSummary task={task('Pending')} />)
    expect(screen.getByText('Derived checks')).toBeInTheDocument()
    expect(screen.getByText('Not a formal evaluation')).toBeInTheDocument()
  })

  it('lets the rollup badge wrap inside a narrow card instead of overflowing it', () => {
    render(<ValidationSummary task={task('Failed', { message: 'boom' })} trace={trace()} />)
    const badge = screen.getByText('Derived checks failing')
    expect(badge).toHaveClass('shrink', 'whitespace-normal')
    expect(badge).not.toHaveClass('shrink-0', 'whitespace-nowrap')
    expect(badge.parentElement).toHaveClass('flex-wrap')
  })

  it('rolls up to pass for a clean succeeded task', () => {
    render(
      <ValidationSummary
        task={task('Succeeded', { resultRef: { configMapName: 'r' } })}
        trace={trace()}
        approvals={[]}
        artifacts={[{ filename: 'out.txt' }]}
      />,
    )
    expect(screen.getByText('All derived checks pass')).toBeInTheDocument()
    expect(screen.getByText('Reached terminal state')).toBeInTheDocument()
    expect(screen.getByText('No errors')).toBeInTheDocument()
  })

  it('does not drag a succeeded task without artifacts to unknown', () => {
    render(
      <ValidationSummary
        task={task('Succeeded', { resultRef: { available: true } })}
        trace={trace()}
        approvals={[]}
        artifacts={[]}
      />,
    )
    expect(screen.getByText('All derived checks pass')).toBeInTheDocument()
    expect(screen.getByText('None published')).toBeInTheDocument()
    expect(screen.getByText(/— Info/)).toBeInTheDocument()
  })

  it('does not flag a cleanly running task as needing attention', () => {
    render(<ValidationSummary task={task('Running')} trace={trace()} approvals={[]} />)
    expect(screen.queryByText('Derived checks need attention')).not.toBeInTheDocument()
    expect(screen.getByText('Phase Running; still in progress')).toBeInTheDocument()
  })

  it('flags a running task stuck on an image pull instead of reading all-pass', () => {
    const message = 'pod waiting: ImagePullBackOff - Back-off pulling image "ghcr.io/example/missing:v1"'
    render(<ValidationSummary task={task('Running', { message })} trace={trace()} approvals={[]} />)
    expect(screen.getByText('Derived checks need attention')).toBeInTheDocument()
    expect(screen.queryByText('All derived checks pass')).not.toBeInTheDocument()
    expect(screen.getByText(`Phase Running; ${message}`)).toBeInTheDocument()
  })

  it('rolls up to fail when the trace has errors', () => {
    render(
      <ValidationSummary
        task={task('Failed')}
        trace={trace({ errors: [{ message: 'boom' }] })}
      />,
    )
    expect(screen.getByText('Derived checks failing')).toBeInTheDocument()
    expect(screen.getByText('1 error(s)')).toBeInTheDocument()
  })

  it('rolls up to warn when an approval is pending', () => {
    const approvals: Approval[] = [
      { id: 'a1', action: 'bash', status: 'pending', createdAt: new Date().toISOString() },
    ]
    render(<ValidationSummary task={task('Running')} trace={trace()} approvals={approvals} />)
    expect(screen.getByText('Derived checks need attention')).toBeInTheDocument()
    expect(screen.getByText('1 blocking')).toBeInTheDocument()
  })

  it('reads unknown when trace data is unavailable', () => {
    render(<ValidationSummary task={task('Pending')} />)
    expect(screen.getByText('Derived health unknown')).toBeInTheDocument()
    expect(screen.getAllByText('Trace unavailable').length).toBeGreaterThan(0)
  })

  it('keeps the failing rollup badge foreground readable', () => {
    render(<ValidationSummary task={task('Failed')} trace={trace({ errors: [{ message: 'boom' }] })} />)
    const badge = screen.getByText('Derived checks failing').closest('[data-slot="badge"]')
    expect(badge?.className).toContain('text-destructive-foreground')
    expect(badge?.className).not.toContain('text-status-failed')
  })

  it('conveys status with sr-only text, not color alone', () => {
    render(<ValidationSummary task={task('Failed')} trace={trace({ errors: [{ message: 'x' }] })} />)
    expect(screen.getAllByText(/— Fail/).length).toBeGreaterThan(0)
  })
})
