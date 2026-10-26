import { describe, expect, it } from 'vitest'
import { enforcementRequirementSchema, taskSchema, type Task } from '@/schemas/task'
import { displayedTaskMessage, MAX_ENFORCEMENT_INPUT_BYTES, parseTaskEnforcement, taskEnforcementObservation } from './task-enforcement'

const requirement = {
  mode: 'enforce' as const,
  format: 'agentsuite.policy/v3',
  policy: 'AP8=',
  requiredCapabilities: ['network/egress-allow/v1', 'filesystem/deny-write/v1'],
}
const admission = {
  bindingID: `container:${'a'.repeat(64)}`,
  requirementDigest: `sha256:${'b'.repeat(64)}`,
  observedGeneration: 2,
}
const task: Task = {
  metadata: { name: 'protected', generation: 2 },
  spec: { type: 'ai', enforcement: requirement },
  status: {
    enforcementAdmission: admission,
    conditions: [{
      type: 'EnforcementReady', status: 'True',
      reason: 'EnforcementVerified', observedGeneration: 2,
      message: 'arbitrary-provider-private-prose',
    }],
  },
}

describe('complete enforcement input', () => {
  it('preserves opaque policy bytes and capability order through input and Task parsing', () => {
    expect(parseTaskEnforcement(JSON.stringify(requirement))).toEqual({ requirement })
    expect(taskSchema.parse(task).spec.enforcement).toEqual(requirement)
    expect(parseTaskEnforcement(' \n')).toEqual({ requirement: undefined })
  })

  it.each([
    { ...requirement, mode: 'advisory' },
    { ...requirement, policy: '%%%%' },
    { ...requirement, policy: 'AB==' },
    { ...requirement, policy: 'AA' },
    { ...requirement, policy: '' },
    { ...requirement, format: ' spaced ' },
    { ...requirement, format: 'é'.repeat(129) },
    { ...requirement, requiredCapabilities: [] },
    { ...requirement, requiredCapabilities: ['same', 'same'] },
    { ...requirement, requiredCapabilities: Array.from({ length: 33 }, (_, index) => `capability-${index}`) },
    { ...requirement, endpoint: 'untrusted-provider-routing' },
    { mode: 'enforce' },
  ])('refuses incomplete, invalid or silently extended input without echoing it: %#', (value) => {
    const result = parseTaskEnforcement(JSON.stringify(value))
    expect(result.error).toBeDefined()
    expect(result.requirement).toBeUndefined()
    expect(result.error).not.toContain('untrusted-provider-routing')
  })

  it('accepts exactly 512 KiB of opaque bytes but refuses one extra decoded byte', () => {
    const maximum = { ...requirement, policy: btoa('\0'.repeat(512 * 1024)) }
    expect(enforcementRequirementSchema.parse(maximum)).toEqual(maximum)
    expect(enforcementRequirementSchema.safeParse({
      ...requirement, policy: btoa('\0'.repeat(512 * 1024 + 1)),
    }).success).toBe(false)
  })

  it('bounds the whole JSON input and hides syntax-error source excerpts', () => {
    expect(parseTaskEnforcement(' '.repeat(MAX_ENFORCEMENT_INPUT_BYTES) + '{}').error).toBeDefined()
    expect(parseTaskEnforcement('{"policy": "private-broken-source",').error)
      .toBe('Enforcement requirement must be valid JSON.')
  })
})

describe('historical enforcement projection', () => {
  it('requires current-generation verified admission and keeps only validated receipt fields', () => {
    expect(taskEnforcementObservation(task)).toMatchObject({
      state: 'Controller admission recorded.', admission,
    })
    expect(taskEnforcementObservation(taskSchema.parse(task))).toMatchObject({
      state: 'Controller admission recorded.', admission,
    })
    expect(taskSchema.parse(task).status?.enforcementAdmission).toEqual(admission)
    expect(taskEnforcementObservation({
      ...task, metadata: { ...task.metadata, generation: 3 },
    })?.state).toBe('Admission not confirmed.')
  })

  it('does not upgrade old receipts or unvalidated provider text into current admission', () => {
    expect(taskEnforcementObservation({
      ...task,
      status: { ...task.status, enforcementAdmission: { ...admission, observedGeneration: 1 } },
    })?.state).toBe('Admission reported; physical receipt unavailable.')
    const result = taskEnforcementObservation({
      ...task,
      status: { ...task.status, enforcementAdmission: { ...admission, bindingID: 'private-provider-handle' } },
    })
    expect(result?.admission).toBeUndefined()
    expect(JSON.stringify(result)).not.toMatch(/private-provider-handle|arbitrary-provider-private-prose/)
  })

  it('retains a prior receipt without claiming it applies to a Task without a requirement', () => {
    expect(taskEnforcementObservation({ ...task, spec: { type: 'ai' } })?.state)
      .toBe('No current requirement; prior admission record only.')
    expect(taskEnforcementObservation({ ...task, spec: { type: 'ai' }, status: undefined })).toBeUndefined()
  })

  it.each([
    ['False', 'EnforcementRefused', 'Enforcement refused; admission not confirmed.'],
    ['False', 'EnforcementPending', 'Waiting for enforcement; admission not confirmed.'],
    ['Unknown', 'EnforcementUncertain', 'Enforcement uncertain; admission not confirmed.'],
  ])('projects the current %s/%s state without arbitrary messages', (status, reason, state) => {
    const result = taskEnforcementObservation({
      ...task,
      status: {
        conditions: [{
          type: 'EnforcementReady', status, reason, observedGeneration: 2,
          message: 'private-provider-policy-and-credentials',
        }],
      },
    })
    expect(result).toMatchObject({ state, admission: undefined })
  })
})

describe('safe actionable enforcement outcomes', () => {
  const outcome = { observedGeneration: 2, reason: 'EnforcementRefused', needs: ['supported-format', 'complete-policy'] }
  const blocked: Task = {
    ...task,
    status: {
      phase: 'Failed', message: 'private-provider-prose',
      conditions: [{ type: 'EnforcementReady', status: 'False', reason: 'EnforcementRefused', observedGeneration: 2 }],
      enforcementOutcome: outcome,
    },
  }

  it('projects current needs as fixed guidance without adopting provider prose', () => {
    const observation = taskEnforcementObservation(blocked)
    expect(observation?.outcome).toEqual(outcome)
    expect(observation?.guidance.join(' ')).toMatch(/format supported/)
    expect(observation?.guidance.join(' ')).toMatch(/complete supported policy/)
    expect(displayedTaskMessage(blocked, blocked.status?.message)).not.toContain('private-provider')
    expect(displayedTaskMessage(blocked, blocked.status?.message)).toMatch(/format supported/)
  })

  it.each([
    'private-provider-details',
    { ...outcome, observedGeneration: 1 },
    { ...outcome, reason: 'EnforcementPending' },
    { ...outcome, needs: ['private-provider-details'] },
    { ...outcome, needs: 'private-provider-details' },
    { ...outcome, message: 'private-provider-details' },
  ])('keeps Tasks readable but does not use malformed or stale optional detail: %#', (enforcementOutcome) => {
    const source = { ...blocked, status: { ...blocked.status, enforcementOutcome } }
    const parsed = taskSchema.parse(source)
    expect(parsed.metadata.name).toBe(blocked.metadata.name)
    const observation = taskEnforcementObservation(parsed)
    expect(observation?.outcome).toBeUndefined()
    expect(observation?.state).toMatch(/refused/)
    expect(observation?.guidance.join(' ')).toMatch(/unavailable or stale/)
    expect(JSON.stringify(observation)).not.toContain('private-provider')
  })

  it('keeps ordinary and successfully admitted Task messages unchanged', () => {
    expect(displayedTaskMessage({ ...task, spec: { type: 'ai' }, status: undefined }, 'ordinary failure')).toBe('ordinary failure')
    expect(displayedTaskMessage(task, 'ordinary workload failure after admission')).toBe('ordinary workload failure after admission')
  })

  it.each([
    'private-invalid-receipt',
    { ...admission, bindingID: 'private-invalid-binding' },
    { ...admission, requirementDigest: 'private-invalid-digest' },
  ])('keeps a Task readable when its optional historical receipt is malformed: %#', (enforcementAdmission) => {
    const parsed = taskSchema.parse({ ...task, status: { ...task.status, enforcementAdmission } })
    const observation = taskEnforcementObservation(parsed)
    expect(observation?.state).toBe('Admission reported; physical receipt unavailable.')
    expect(observation?.admission).toBeUndefined()
    expect(JSON.stringify(observation)).not.toContain('private-invalid')
  })

  it.each([
    'pod waiting: ImagePullBackOff - pull access denied',
    'pod stuck: CreateContainerConfigError - missing Secret',
    'pod unschedulable: insufficient CPU',
  ])('preserves separately identified infrastructure diagnostics during enforcement preparation: %s', (message) => {
    const source: Task = {
      ...blocked, status: { ...blocked.status, phase: 'Running', message },
    }
    expect(displayedTaskMessage(source, message)).toBe(message)
  })
})
