import type { Task, TaskPhase } from '@/schemas/task'
import type { TaskTrace, Approval } from '@/schemas/execution-event'
import type { ArtifactMetadata } from '@/schemas/artifact'
import { podWaitingMessage } from '@/components/tasks/task-state'

/**
 * Derived runtime health checks — NOT a formal evaluation. These summarize
 * task/system health from real Orka status + trace data. Missing data yields
 * 'unknown', never a false pass. 'info' is a neutral observation (a task
 * still in progress, a task without artifacts) that does not affect the
 * rollup. Pure functions, no fetching.
 */
export type CheckStatus = 'pass' | 'fail' | 'warn' | 'unknown' | 'info'
export type RollupStatus = Exclude<CheckStatus, 'info'>

export interface RuntimeCheck {
  id: string
  label: string
  status: CheckStatus
  reason: string
}

const TERMINAL: TaskPhase[] = ['Succeeded', 'Failed', 'Cancelled']

export function isTerminal(phase?: TaskPhase | string): boolean {
  return TERMINAL.includes(phase as TaskPhase)
}

export interface ValidationInput {
  task: Task
  trace?: TaskTrace
  approvals?: Approval[]
  artifacts?: ArtifactMetadata[]
}

/**
 * Derive the check list. Order is stable for deterministic rendering. Each check
 * degrades to 'unknown' when its backing data is absent rather than passing.
 */
export function deriveRuntimeChecks({ task, trace, approvals, artifacts }: ValidationInput): RuntimeCheck[] {
  const phase = task.status?.phase
  const checks: RuntimeCheck[] = []

  // A Failed or Cancelled task did reach a terminal state, so that check
  // passes and the outcome is its own check. A task still in progress is
  // informational, not something that needs attention, unless its Pod is
  // stuck waiting (an image pull or config failure): that needs a person.
  const podWaiting = podWaitingMessage(task)
  checks.push({
    id: 'terminal',
    label: 'Reached terminal state',
    status: isTerminal(phase) ? 'pass' : podWaiting ? 'warn' : phase ? 'info' : 'unknown',
    reason: !phase
      ? 'No phase yet'
      : isTerminal(phase)
        ? `Phase ${phase}`
        : podWaiting
          ? `Phase ${phase}; ${podWaiting}`
          : `Phase ${phase}; still in progress`,
  })
  // The authoritative phase drives the outcome: Failed/Cancelled must
  // fail/warn regardless of trace contents, so a terminal failure can't read
  // as "pass" just because the trace is empty or stale.
  if (isTerminal(phase)) {
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
    // Go marshals empty trace slices as null, so guard every array before use
    // (matches TaskTraceView) — these run on the default Runtime tab.
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
    // ResultReference serializes as { available }. For legacy shapes, require
    // a concrete field; an empty tolerated object must not count as a result.
    const ref = task.status?.resultRef
    const legacyRefAvailable = ref?.available === undefined && Boolean(ref?.configMapName || ref?.key)
    const hasResult = ref?.available === true || legacyRefAvailable || Boolean(trace?.task.resultAvailable)
    checks.push({
      id: 'result',
      label: 'Result available',
      status: hasResult ? 'pass' : 'warn',
      reason: hasResult ? 'Result present' : 'No result recorded',
    })
  }

  if (artifacts !== undefined) {
    // Many tasks legitimately publish no artifacts, so their absence is
    // informational rather than unknown health.
    checks.push({
      id: 'artifacts',
      label: 'Artifacts',
      status: artifacts.length > 0 ? 'pass' : 'info',
      reason: artifacts.length > 0 ? `${artifacts.length} artifact(s)` : 'None published',
    })
  }

  return checks
}

/**
 * Roll up checks into a single banner status (fail > warn > unknown > pass).
 * Informational checks never move the rollup.
 */
export function rollupStatus(checks: RuntimeCheck[]): RollupStatus {
  const scored = checks.filter((c) => c.status !== 'info')
  if (scored.some((c) => c.status === 'fail')) return 'fail'
  if (scored.some((c) => c.status === 'warn')) return 'warn'
  if (scored.length > 0 && scored.every((c) => c.status === 'pass')) return 'pass'
  return 'unknown'
}
