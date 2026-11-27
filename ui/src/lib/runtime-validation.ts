import type { Task } from '@/schemas/task'
import type { TaskTrace, Approval } from '@/schemas/execution-event'
import type { ArtifactMetadata } from '@/schemas/artifact'
import { podWaitingMessage } from '@/components/tasks/task-state'
import { isTaskTerminal } from '@/lib/task-phase'

export type CheckStatus = 'pass' | 'fail' | 'warn' | 'unknown' | 'info'
export type RollupStatus = Exclude<CheckStatus, 'info'>

export interface RuntimeCheck {
  id: string
  label: string
  status: CheckStatus
  reason: string
}

export interface ValidationInput {
  task: Task
  trace?: TaskTrace
  approvals?: Approval[]
  artifacts?: ArtifactMetadata[]
}

export function deriveRuntimeChecks({ task, trace, approvals, artifacts }: ValidationInput): RuntimeCheck[] {
  const phase = task.status?.phase
  const checks: RuntimeCheck[] = []

  const podWaiting = podWaitingMessage(task)
  checks.push({
    id: 'terminal',
    label: 'Reached terminal state',
    status: isTaskTerminal(phase) ? 'pass' : podWaiting ? 'warn' : phase ? 'info' : 'unknown',
    reason: !phase
      ? 'No phase yet'
      : isTaskTerminal(phase)
        ? `Phase ${phase}`
        : podWaiting
          ? `Phase ${phase}; ${podWaiting}`
          : `Phase ${phase}; still in progress`,
  })
  if (isTaskTerminal(phase)) {
    checks.push({
      id: 'outcome',
      label: 'Task succeeded',
      status: phase === 'Succeeded' ? 'pass' : phase === 'Failed' ? 'fail' : 'warn',
      reason: phase === 'Succeeded' ? 'Succeeded' : task.status?.message || `Phase ${phase}`,
    })
  }

  if (!trace) {
    checks.push({ id: 'errors', label: 'No trace errors', status: 'unknown', reason: 'Trace unavailable' })
    checks.push({ id: 'warnings', label: 'No trace warnings', status: 'unknown', reason: 'Trace unavailable' })
  } else {
    const errors = trace.errors ?? []
    const warnings = trace.warnings ?? []
    const modelRequests = trace.modelRequests ?? []
    const toolCalls = trace.toolCalls ?? []
    checks.push({
      id: 'errors',
      label: 'No trace errors',
      status: errors.length === 0 ? 'pass' : 'fail',
      reason: errors.length === 0 ? 'No errors' : `${errors.length} error(s)`,
    })
    checks.push({
      id: 'warnings',
      label: 'No trace warnings',
      status: warnings.length === 0 ? 'pass' : 'warn',
      reason: warnings.length === 0 ? 'No warnings' : `${warnings.length} warning(s)`,
    })
    const badModel = modelRequests.filter((m) => m.status === 'failed').length
    checks.push({
      id: 'model',
      label: 'No failed model requests',
      status: badModel === 0 ? 'pass' : 'fail',
      reason: badModel === 0 ? 'All model requests ok' : `${badModel} failed`,
    })
    const badTool = toolCalls.filter((t) => t.status === 'failed').length
    checks.push({
      id: 'tools',
      label: 'No failed tool calls',
      status: badTool === 0 ? 'pass' : 'fail',
      reason: badTool === 0 ? 'All tool calls ok' : `${badTool} failed`,
    })
  }

  const pending = (approvals ?? []).filter((a) => a.status === 'pending').length
  checks.push({
    id: 'approvals',
    label: 'No pending approvals',
    status: approvals === undefined ? 'unknown' : pending === 0 ? 'pass' : 'warn',
    reason: approvals === undefined ? 'Approvals unavailable' : pending === 0 ? 'None pending' : `${pending} blocking`,
  })

  if (phase === 'Succeeded') {
    const hasResult = task.status?.resultRef?.available === true || Boolean(trace?.task.resultAvailable)
    checks.push({
      id: 'result',
      label: 'Result available',
      status: hasResult ? 'pass' : 'warn',
      reason: hasResult ? 'Result present' : 'No result recorded',
    })
  }

  if (artifacts !== undefined) {
    checks.push({
      id: 'artifacts',
      label: 'Artifacts',
      status: artifacts.length > 0 ? 'pass' : 'info',
      reason: artifacts.length > 0 ? `${artifacts.length} artifact(s)` : 'None published',
    })
  }

  return checks
}

export function rollupStatus(checks: RuntimeCheck[]): RollupStatus {
  const scored = checks.filter((c) => c.status !== 'info')
  if (scored.some((c) => c.status === 'fail')) return 'fail'
  if (scored.some((c) => c.status === 'warn')) return 'warn'
  if (scored.length > 0 && scored.every((c) => c.status === 'pass')) return 'pass'
  return 'unknown'
}
