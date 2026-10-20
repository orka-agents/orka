import { describe, it, expect } from 'vitest'
import { deriveRuntimeChecks, rollupStatus, isTerminal } from './runtime-validation'
import type { Task } from '@/schemas/task'
import type { TaskTrace } from '@/schemas/execution-event'

const base: Task = { metadata: { name: 't', namespace: 'default' }, spec: { type: 'agent' }, status: { phase: 'Running' } }

const trace = (o: Partial<TaskTrace> = {}): TaskTrace => ({
  task: { namespace: 'default', name: 't', resultAvailable: false },
  latestSeq: 1, generatedAt: '', timeline: [], modelRequests: [], toolCalls: [],
  childTasks: [], workspace: [], artifacts: [], errors: [], warnings: [], ...o,
})

describe('runtime-validation', () => {
  it('isTerminal', () => {
    expect(isTerminal('Succeeded')).toBe(true)
    expect(isTerminal('Running')).toBe(false)
  })

  it('marks trace checks unknown when no trace', () => {
    const checks = deriveRuntimeChecks({ task: { ...base, status: { phase: 'Pending' } } })
    expect(checks.find((c) => c.id === 'errors')?.status).toBe('unknown')
    expect(rollupStatus(checks)).toBe('unknown')
  })

  it('failed task with errors fails', () => {
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Failed' } },
      trace: trace({ errors: [{ message: 'boom' }] }),
    })
    expect(checks.find((c) => c.id === 'errors')?.status).toBe('fail')
    expect(rollupStatus(checks)).toBe('fail')
  })

  it('succeeded clean task mostly passes', () => {
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Succeeded', resultRef: { configMapName: 'r' } } },
      trace: trace({ task: { namespace: 'default', name: 't', resultAvailable: true } }),
      approvals: [], artifacts: [{ filename: 'a' }],
    })
    expect(rollupStatus(checks)).toBe('pass')
    expect(checks.find((c) => c.id === 'result')?.status).toBe('pass')
  })

  it('does not treat an empty resultRef as an available result', () => {
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Succeeded', resultRef: {} } },
      trace: trace({ task: { namespace: 'default', name: 't', resultAvailable: false } }),
    })
    const result = checks.find((c) => c.id === 'result')
    expect(result?.status).toBe('warn')
    expect(result?.reason).toBe('No result recorded')
  })

  it('pending approval warns', () => {
    const checks = deriveRuntimeChecks({ task: base, approvals: [{ id: '1', action: 'x', status: 'pending', createdAt: '' }] })
    expect(checks.find((c) => c.id === 'approvals')?.status).toBe('warn')
  })

  it('failed task with empty trace never rolls up to pass', () => {
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Failed' } },
      trace: trace(), // no errors recorded, but phase is authoritative
    })
    // The task did reach a terminal state; the failure is its own check.
    expect(checks.find((c) => c.id === 'terminal')?.status).toBe('pass')
    expect(checks.find((c) => c.id === 'outcome')?.status).toBe('fail')
    expect(rollupStatus(checks)).toBe('fail')
  })

  it('tolerates null trace arrays from the API (no crash)', () => {
    const nullish = { task: { namespace: 'default', name: 't', resultAvailable: false }, latestSeq: 1, generatedAt: '', timeline: [], childTasks: [], workspace: [], artifacts: [], errors: null, warnings: null, modelRequests: null, toolCalls: null } as unknown as TaskTrace
    const checks = deriveRuntimeChecks({ task: { ...base, status: { phase: 'Succeeded' } }, trace: nullish })
    expect(checks.find((c) => c.id === 'errors')?.status).toBe('pass')
    expect(checks.find((c) => c.id === 'model')?.status).toBe('pass')
  })

  it('cancelled task warns, not pass', () => {
    const checks = deriveRuntimeChecks({ task: { ...base, status: { phase: 'Cancelled' } }, trace: trace() })
    expect(rollupStatus(checks)).not.toBe('pass')
  })

  it('reports a failed task as terminal with a separate failed outcome carrying the message', () => {
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Failed', message: 'container exited 1' } },
      trace: trace(),
    })
    expect(checks.find((c) => c.id === 'terminal')).toMatchObject({ status: 'pass', reason: 'Phase Failed' })
    expect(checks.find((c) => c.id === 'outcome')).toMatchObject({ status: 'fail', reason: 'container exited 1' })
  })

  it('a running task with clean data does not need attention', () => {
    const checks = deriveRuntimeChecks({ task: base, trace: trace(), approvals: [], artifacts: [] })
    expect(checks.find((c) => c.id === 'terminal')?.status).toBe('info')
    expect(checks.find((c) => c.id === 'outcome')).toBeUndefined()
    expect(rollupStatus(checks)).toBe('pass')
  })

  it('a running task whose Pod is stuck waiting needs attention instead of passing', () => {
    const message = 'pod waiting: ImagePullBackOff - Back-off pulling image "ghcr.io/example/missing:v1"'
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Running', message } },
      trace: trace(), approvals: [], artifacts: [],
    })
    expect(checks.find((c) => c.id === 'terminal')).toMatchObject({ status: 'warn', reason: `Phase Running; ${message}` })
    expect(rollupStatus(checks)).toBe('warn')
  })

  it('a succeeded task without artifacts still rolls up to pass', () => {
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Succeeded', resultRef: { available: true } } },
      trace: trace(),
      approvals: [],
      artifacts: [],
    })
    expect(checks.find((c) => c.id === 'artifacts')).toMatchObject({ status: 'info', reason: 'None published' })
    expect(rollupStatus(checks)).toBe('pass')
  })

  it('a rollup of only informational checks is unknown, not pass', () => {
    expect(rollupStatus([{ id: 'a', label: 'a', status: 'info', reason: '' }])).toBe('unknown')
  })
})
