import type { Task } from '@/schemas/task'

/**
 * Pure selection/grouping helpers for the Runtime Canvas (Slice A).
 *
 * The Orka API is the source of truth: these functions only ever derive a view
 * from a task list (and optional per-task latest-activity hints). They never
 * mutate, never synthesize tasks, and never assume `status.startTime` is
 * present — every ordering has an explicit deterministic fallback so a running
 * task with no startTime can't push `undefined` into `Date`/`localeCompare`.
 */

/** Bucket name for tasks with no `spec.agentRef.name`. */
export const UNASSIGNED_AGENT = 'unassigned'

/** Phases considered "in-flight" for spotlight/roster purposes. */
export function isLiveTask(task: Task): boolean {
  return task.status?.phase === 'Running'
}

/** Stable identity for a task row (uid preferred, name as fallback). */
export function taskKey(task: Task): string {
  return task.metadata.uid || task.metadata.name
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

/** Latest observed event time (epoch ms), keyed by task name. Higher = more recent. */
export type LatestActivityByTask = Record<string, number>

/**
 * Deterministic ordering for "most recently active" first. Tie-breaks descend a
 * fixed ladder so the result is stable even when fields are missing:
 *   1. latest event time (when provided)  — what just advanced
 *   2. status.startTime                    — newest start
 *   3. metadata.creationTimestamp          — newest created
 *   4. metadata.name (ascending)           — stable final tiebreak
 */
export function compareByActivity(
  a: Task,
  b: Task,
  latestActivity: LatestActivityByTask = {},
): number {
  const activityA = latestActivity[a.metadata.name] ?? -1
  const activityB = latestActivity[b.metadata.name] ?? -1
  if (activityA !== activityB) return activityB - activityA

  const startA = epochMs(a.status?.startTime)
  const startB = epochMs(b.status?.startTime)
  if (startA !== startB) return (startB ?? -1) - (startA ?? -1)

  const createA = epochMs(a.metadata.creationTimestamp)
  const createB = epochMs(b.metadata.creationTimestamp)
  if (createA !== createB) return (createB ?? -1) - (createA ?? -1)

  return a.metadata.name.localeCompare(b.metadata.name)
}

/**
 * Pick the spotlight task: the most recently active running task, by
 * compareByActivity. Returns null only when nothing is running (a clear idle
 * state, never a misleading non-running pick).
 */
export function selectActiveTask(
  tasks: Task[],
  latestActivity: LatestActivityByTask = {},
): Task | null {
  const running = tasks.filter(isLiveTask)
  if (running.length === 0) return null
  return [...running].sort((a, b) => compareByActivity(a, b, latestActivity))[0]
}

export interface AgentGroup {
  agent: string
  tasks: Task[]
}

/**
 * Group tasks by `spec.agentRef.name`, placing agent-less tasks under
 * UNASSIGNED_AGENT. Groups are activity-sorted; tasks within a group too.
 * Unassigned always sorts last so named agents lead.
 */
export function groupTasksByAgent(
  tasks: Task[],
  latestActivity: LatestActivityByTask = {},
): AgentGroup[] {
  const byAgent = new Map<string, Task[]>()
  for (const task of tasks) {
    const agent = task.spec.agentRef?.name || UNASSIGNED_AGENT
    const bucket = byAgent.get(agent)
    if (bucket) bucket.push(task)
    else byAgent.set(agent, [task])
  }
  const groups = Array.from(byAgent, ([agent, group]) => ({
    agent,
    tasks: [...group].sort((a, b) => compareByActivity(a, b, latestActivity)),
  }))
  return groups.sort((a, b) => {
    if (a.agent === UNASSIGNED_AGENT) return 1
    if (b.agent === UNASSIGNED_AGENT) return -1
    return compareByActivity(a.tasks[0], b.tasks[0], latestActivity)
  })
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
