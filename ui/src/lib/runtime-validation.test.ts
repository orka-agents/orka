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

  it('pending approval warns', () => {
    const checks = deriveRuntimeChecks({ task: base, approvals: [{ id: '1', action: 'x', status: 'pending', createdAt: '' }] })
    expect(checks.find((c) => c.id === 'approvals')?.status).toBe('warn')
  })

  it('failed task with empty trace never rolls up to pass', () => {
    const checks = deriveRuntimeChecks({
      task: { ...base, status: { phase: 'Failed' } },
      trace: trace(), // no errors recorded, but phase is authoritative
    })
    expect(checks.find((c) => c.id === 'terminal')?.status).toBe('fail')
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
})
