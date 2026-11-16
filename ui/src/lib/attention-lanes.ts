import type { AttentionItem, AttentionKind } from '@/schemas/attention'
import { humanizeDuration } from '@/lib/time'
import type { Task } from '@/schemas/task'

export type AttentionLane = 'needsYou' | 'working' | 'done'

export const attentionLanes: { lane: AttentionLane; label: string; description: string }[] = [
  { lane: 'needsYou', label: 'Needs you', description: 'Approvals, unknown outcomes, failures and stuck Pods' },
  { lane: 'working', label: 'Agent working', description: 'Queued, running, or waiting on capacity, child Tasks or a timer' },
  { lane: 'done', label: 'Done', description: 'Everything else' },
]

const workingPhases = new Set(['', 'Pending', 'Scheduled', 'Running', 'Finalizing'])
const activeWaitStates = new Set(['Requested', 'Waiting', 'Resuming'])

/** A Task the agent (or the platform on its behalf) is still moving forward. */
export function isAgentWorking(task: Task): boolean {
  if (workingPhases.has(task.status?.phase ?? '')) return true
  if (task.status?.conditions?.some((c) => c.type === 'WaitingForCapacity' && c.status === 'True')) return true
  const wait = task.status?.wait
  return (wait?.kind === 'Tasks' || wait?.kind === 'Timer') && activeWaitStates.has(wait.state ?? '')
}

/** Task UIDs (falling back to names) that have an attention item. */
export function attentionTaskKeys(items: AttentionItem[]): Set<string> {
  const keys = new Set<string>()
  for (const item of items) {
    if (item.task) keys.add(item.task.uid || item.task.name)
  }
  return keys
}

function taskKey(task: Task): string {
  return task.metadata.uid || task.metadata.name
}

/**
 * Groups Tasks into the attention lanes. "Needs you" is driven only by the
 * server's attention items (which also carry non-Task items such as blocked
 * monitor actions), so a Task listed there is left out of the other lanes.
 */
export function groupByAttention(tasks: Task[], items: AttentionItem[]): { needsYou: AttentionItem[]; working: Task[]; done: Task[] } {
  const flagged = attentionTaskKeys(items)
  const working: Task[] = []
  const done: Task[] = []
  for (const task of tasks) {
    if (flagged.has(taskKey(task))) continue
    if (isAgentWorking(task)) working.push(task)
    else done.push(task)
  }
  return { needsYou: items, working, done }
}

const incompleteReasonText: Record<string, string> = {
  taskScanLimit: 'only the first Tasks in the namespace were scanned',
  approvalCandidateLimit: 'too many Tasks were waiting for approval to read them all',
  approvalEventLimit: 'a Task had too much approval history to read',
  monitorActionLimit: 'too many blocked monitor actions to read them all',
}

/** A readable sentence for an incomplete ranking, never shown as complete. */
export function incompleteText(reasons: string[] | undefined): string {
  const parts = (reasons ?? []).map((reason) => incompleteReasonText[reason] ?? reason)
  return parts.length > 0 ? `This list may be incomplete: ${parts.join('; ')}.` : 'This list may be incomplete.'
}

export const attentionKindLabels: Record<AttentionKind, string> = {
  approval: 'Approval',
  outcomeUnknown: 'Outcome unknown',
  failed: 'Failed',
  podWaiting: 'Pod stuck',
  monitorBlocked: 'Monitor blocked',
}

/** "due in 12m", or "overdue" once the deadline passed between polls. */
export function deadlineText(deadline?: string, now = Date.now()): string | undefined {
  if (!deadline) return undefined
  const ms = new Date(deadline).getTime()
  if (Number.isNaN(ms)) return undefined
  const remaining = (ms - now) / 1000
  return remaining <= 0 ? 'overdue' : `due in ${humanizeDuration(remaining)}`
}

export type KanbanView = 'phase' | 'attention'

/** Normalizes the board's ?view= search parameter; anything unknown is the phase view. */
export function parseKanbanView(value: unknown): KanbanView {
  return value === 'attention' ? 'attention' : 'phase'
}
