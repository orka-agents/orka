import {
  enforcementAdmissionSchema,
  enforcementOutcomeSchema,
  enforcementRequirementSchema,
  type EnforcementAdmission,
  type EnforcementNeed,
  type EnforcementOutcome,
  type EnforcementRequirement,
  type Task,
} from '@/schemas/task'

export const MAX_ENFORCEMENT_INPUT_BYTES = 1024 * 1024

type RequirementInput =
  | { requirement: EnforcementRequirement | undefined; error?: never }
  | { error: string; requirement?: never }

export function parseTaskEnforcement(text: string): RequirementInput {
  if (!text.trim()) return { requirement: undefined }
  if (new TextEncoder().encode(text).length > MAX_ENFORCEMENT_INPUT_BYTES) {
    return { error: 'Enforcement requirement JSON must not exceed 1 MiB.' }
  }
  let value: unknown
  try {
    value = JSON.parse(text)
  } catch (error) {
    if (!(error instanceof SyntaxError)) throw error
    return { error: 'Enforcement requirement must be valid JSON.' }
  }
  const parsed = enforcementRequirementSchema.safeParse(value)
  if (!parsed.success) {
    return { error: 'Provide exactly mode "enforce", format, a canonical base64 policy of at most 512 KiB, and 1-32 distinct requiredCapabilities.' }
  }
  return { requirement: parsed.data }
}

const needGuidance: Record<EnforcementNeed, string> = {
  'complete-policy': 'Supply one complete supported policy; do not omit a required capability.',
  'supported-format': 'Use a policy format supported by every mandatory provider.',
  'configured-backend': 'Ask the operator to configure the native backend for the requested policy.',
  'supported-target': 'Use a supported prepared container and filesystem/network profile.',
  'supported-runtime': 'Use the managed native AI worker or a supported managed ACP RuntimePool.',
  'mandatory-capability-coverage': 'Ask the operator to configure mandatory providers covering every requested capability.',
  'provider-recovery': 'Wait for the configured providers and prepared runtime to recover; do not weaken the requirement.',
  'prepared-container': 'Wait for the exact managed worker container to become prepared.',
  'stopped-previous-container': 'Wait for the previous physical container to stop and its controls to retire.',
  'operator-configuration': 'Ask the operator to check provider enrollment, policy and target compatibility.',
  'admission-recovery': 'Inspect the Task admission recovery; a missing receipt does not authorize removal of controls.',
}

interface EnforcementObservation {
  state: string
  admission?: EnforcementAdmission
  outcome?: EnforcementOutcome
  guidance: string[]
}

export function taskEnforcementObservation(task: Task): EnforcementObservation | undefined {
  const recorded = task.status?.enforcementAdmission
  const diagnostic = task.status?.enforcementOutcome
  const conditions = task.status?.conditions ?? []
  if (!task.spec.enforcement && !recorded && diagnostic === undefined &&
    !conditions.some((condition) => condition.type === 'EnforcementReady')) return undefined
  const receipt = enforcementAdmissionSchema.safeParse(recorded)
  const admission = receipt.success ? receipt.data : undefined
  if (!task.spec.enforcement) {
    return {
      state: recorded ? 'No current requirement; prior admission record only.' : 'No current requirement; prior enforcement observation only.',
      admission, guidance: [],
    }
  }
  if (task.spec.enforcement.mode !== 'enforce') {
    return { state: 'Invalid or unsupported requirement; admission not confirmed.', admission, guidance: [needGuidance['complete-policy']] }
  }
  const condition = task.status?.conditions?.find((value) =>
    value.type === 'EnforcementReady' &&
    (task.metadata.generation ?? 0) > 0 &&
    value.observedGeneration === task.metadata.generation,
  )
  let state = 'Admission not confirmed.'
  if (condition?.status === 'True' && condition.reason === 'EnforcementVerified') {
    state = admission?.observedGeneration === task.metadata.generation
      ? 'Controller admission recorded.'
      : 'Admission reported; physical receipt unavailable.'
  } else if (condition?.status === 'False' && condition.reason === 'EnforcementRefused') {
    state = 'Enforcement refused; admission not confirmed.'
  } else if (condition?.reason === 'EnforcementPending') {
    state = 'Waiting for enforcement; admission not confirmed.'
  } else if (condition?.status === 'Unknown') {
    state = 'Enforcement uncertain; admission not confirmed.'
  }
  const reason = condition?.reason === 'EnforcementReleaseUnobserved' ? 'EnforcementUncertain' : condition?.reason
  const parsed = enforcementOutcomeSchema.safeParse(diagnostic)
  const outcome = condition?.status !== 'True' && parsed.success &&
    parsed.data.observedGeneration === task.metadata.generation &&
    parsed.data.reason === reason ? parsed.data : undefined
  const guidance = outcome?.needs?.length
    ? [...new Set(outcome.needs)].map((need) => needGuidance[need])
    : condition?.status !== 'True'
      ? [needGuidance[reason === 'EnforcementRefused' ? 'operator-configuration' : 'provider-recovery']]
      : []
  if (diagnostic !== undefined && !outcome && condition?.status !== 'True') {
    guidance.unshift('Detailed enforcement remediation is unavailable or stale.')
  }
  return { state, admission, outcome, guidance }
}

export function displayedTaskMessage(task: Task, message: string | undefined): string | undefined {
  if (!message) return message
  if (message === task.status?.message &&
    /^(pod waiting: |pod stuck: |pod unschedulable: |pod stuck in ContainerCreating |pod stuck initializing )/.test(message)) return message
  const blocked = task.status?.enforcementOutcome !== undefined ||
    task.status?.conditions?.some((condition) => condition.type === 'EnforcementReady' && condition.status !== 'True') ||
    (task.spec.enforcement && !task.status?.enforcementAdmission && task.status?.phase === 'Failed')
  if (!blocked) return message
  const observation = taskEnforcementObservation(task)
  return [observation?.state ?? 'Enforcement admission not confirmed.', ...(observation?.guidance ?? [])].join(' ')
}

export function displayedTaskConditionMessage(task: Task, condition: NonNullable<NonNullable<Task['status']>['conditions']>[number]): string | undefined {
  if (condition.type === 'EnforcementReady') return taskEnforcementObservation(task)?.state
  if (condition.reason === 'TaskFailed' || condition.reason?.startsWith('Enforcement') ||
    condition.message === task.status?.message) return displayedTaskMessage(task, condition.message)
  return condition.message
}
