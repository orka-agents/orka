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

  it('conveys status with sr-only text, not color alone', () => {
    render(<ValidationSummary task={task('Failed')} trace={trace({ errors: [{ message: 'x' }] })} />)
    expect(screen.getAllByText(/— Fail/).length).toBeGreaterThan(0)
  })
})
