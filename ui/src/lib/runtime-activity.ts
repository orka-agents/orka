import type { Task } from '@/schemas/task'

/**
 * Pure helpers for the task runtime views. They only derive a view from a Task
 * and never assume `status.startTime` is present, so a running task with no
 * startTime can't push `undefined` into `Date`.
 */

/** Bucket name for tasks with no `spec.agentRef.name`. */
export const UNASSIGNED_AGENT = 'unassigned'

/** Phases considered "in-flight" for the spotlight. */
export function isLiveTask(task: Task): boolean {
  return task.status?.phase === 'Running'
}

/**
 * Parse an RFC3339 timestamp to epoch ms, or null when absent/invalid. Keeps
 * `new Date(undefined)` (NaN) out of comparisons.
 */
function epochMs(ts?: string): number | null {
  if (!ts) return null
  const ms = new Date(ts).getTime()
  return Number.isNaN(ms) ? null : ms
}

/** Whole seconds since startTime, or null when startTime is missing/invalid. */
export function elapsedSeconds(task: Task, nowMs: number): number | null {
  const start = epochMs(task.status?.startTime)
  if (start === null) return null
  return Math.max(0, Math.floor((nowMs - start) / 1000))
}

/** Compact elapsed label; em-dash when no startTime so we never show "0s". */
export function formatElapsed(seconds: number | null): string {
  if (seconds === null) return '—'
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`
}
