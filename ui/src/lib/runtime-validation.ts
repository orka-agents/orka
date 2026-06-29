import type { Task, TaskPhase } from '@/schemas/task'
import type { TaskTrace, Approval } from '@/schemas/execution-event'
import type { ArtifactMetadata } from '@/schemas/artifact'

/**
 * Derived runtime health checks — NOT a formal evaluation. These summarize
 * task/system health from real Orka status + trace data. Missing data yields
 * 'unknown', never a false pass. Pure functions, no fetching.
 */
export type CheckStatus = 'pass' | 'fail' | 'warn' | 'unknown'

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

  // Authoritative phase drives this check: Failed/Cancelled must fail/warn
  // regardless of trace contents, so a terminal failure can't read as "pass"
  // just because the trace is empty or stale.
  checks.push({
    id: 'terminal',
    label: 'Reached terminal state',
    status:
      phase === 'Succeeded' ? 'pass'
      : phase === 'Failed' ? 'fail'
      : phase === 'Cancelled' ? 'warn'
      : phase === 'Running' ? 'warn'
      : 'unknown',
    reason: phase ? `Phase ${phase}` : 'No phase yet',
  })

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
    checks.push({
      id: 'artifacts',
      label: 'Artifacts present',
      status: artifacts.length > 0 ? 'pass' : 'unknown',
      reason: artifacts.length > 0 ? `${artifacts.length} artifact(s)` : 'No artifacts',
    })
  }

  return checks
}

/** Roll up checks into a single banner status (fail > warn > unknown > pass). */
export function rollupStatus(checks: RuntimeCheck[]): CheckStatus {
  if (checks.some((c) => c.status === 'fail')) return 'fail'
  if (checks.some((c) => c.status === 'warn')) return 'warn'
  if (checks.every((c) => c.status === 'pass')) return 'pass'
  return 'unknown'
}
