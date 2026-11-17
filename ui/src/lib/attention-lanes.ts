import type { AttentionItem, AttentionKind, AttentionResponse, AttentionSourceState } from '@/schemas/attention'
import { humanizeDuration } from '@/lib/time'
import { isTaskDeleting, isWaitingForApproval } from '@/components/tasks/task-state'
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
  if ((wait?.kind !== 'Tasks' && wait?.kind !== 'Timer') || !activeWaitStates.has(wait.state ?? '')) return false
  // A requested wait arms only after its turn succeeds; after a failed or
  // cancelled turn it is abandoned and the agent is no longer working.
  return wait.state !== 'Requested' || task.status?.phase === 'Succeeded'
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

/** Summary of a Task-derived Needs you card. */
export const taskApprovalWaitSummary = 'Waiting for approval'

/** ID prefix of Task-derived items, kept apart from the server's item IDs. */
const taskApprovalWaitIdPrefix = 'task-approval-wait:'

/** A Task-derived item that no server ranking produced. */
export function isTaskDerivedItem(item: AttentionItem): boolean {
  return item.id.startsWith(taskApprovalWaitIdPrefix)
}

/**
 * A Task whose own state shows it waiting for a person's approval: the same
 * Task-only test the server uses to pick the Tasks whose approvals it reads
 * (a WaitingForApproval condition or worker message on a live Task, or an
 * armed parked approval wait).
 */
export function isTaskAwaitingApproval(task: Task): boolean {
  return !isTaskDeleting(task) && isWaitingForApproval(task)
}

/**
 * A Needs you card built from the Task alone. It carries no approval: the
 * caller may not be able to read the Task's approvals, and their detail stays
 * on the Task page.
 */
function taskApprovalWaitItem(task: Task): AttentionItem {
  const condition = task.status?.conditions?.find((c) => c.type === 'WaitingForApproval' && c.status === 'True')
  return {
    id: `${taskApprovalWaitIdPrefix}${taskKey(task)}`,
    kind: 'approval',
    // The server's approval rank.
    rank: 1,
    since: condition?.lastTransitionTime ?? task.metadata.creationTimestamp ?? '',
    summary: taskApprovalWaitSummary,
    task: { namespace: task.metadata.namespace ?? '', name: task.metadata.name, uid: task.metadata.uid ?? '', phase: task.status?.phase },
  }
}

/**
 * Groups Tasks into the attention lanes. "Needs you" is driven by the
 * server's attention items (which also carry non-Task items such as blocked
 * monitor actions), so a Task listed there is left out of the other lanes.
 * A Task whose own state shows an approval wait but that has no item (its
 * approvals could not be read, or the ranking is stale) is never put in Agent
 * working or Done: it gets a Task-derived Needs you card, ranked with the
 * approvals and after the server's own.
 */
export function groupByAttention(tasks: Task[], items: AttentionItem[]): { needsYou: AttentionItem[]; working: Task[]; done: Task[] } {
  const flagged = attentionTaskKeys(items)
  const derived: AttentionItem[] = []
  const working: Task[] = []
  const done: Task[] = []
  for (const task of tasks) {
    if (flagged.has(taskKey(task))) continue
    if (isTaskAwaitingApproval(task)) derived.push(taskApprovalWaitItem(task))
    else if (isAgentWorking(task)) working.push(task)
    else done.push(task)
  }
  if (derived.length === 0) return { needsYou: items, working, done }
  // Server items are already rank-ordered; a stable sort keeps their order.
  const needsYou = [...items, ...derived].sort((a, b) => a.rank - b.rank)
  return { needsYou, working, done }
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

export interface AttentionSourceNotes {
  /**
   * Approvals were not all read, so the ranking cannot be trusted for them:
   * an empty list is not "nothing needs you". It is true for every approvals
   * state but included.
   */
  approvalsUnread: boolean
  /** Some approvals were read but not all (sources.approvals is partial). */
  approvalsPartial: boolean
  /** Why approvals are missing, when approvalsUnread is true. */
  approvalsNote?: string
  /** A quiet note for monitor actions that were not read, which is expected without monitor access. */
  monitorsNote?: string
}

export const approvalsUnreadText = 'Approvals could not be read: execution event storage is not enabled on this controller.'
export const approvalsPartialText = 'Some approvals could not be checked: your token cannot read the approvals of every Task that may be waiting for one.'
const approvalsUnknownText = 'Approvals could not be checked.'

function approvalsNoteFor(state: AttentionSourceState | undefined): string | undefined {
  switch (state) {
    case 'included':
      return undefined
    case 'partial':
      return approvalsPartialText
    case 'unavailable':
      return approvalsUnreadText
    default:
      return approvalsUnknownText
  }
}

/** Precedence of approval states across pages: unavailable, then partial, then included. */
const approvalsStateWeight: Partial<Record<AttentionSourceState, number>> = { included: 0, partial: 1 }

/**
 * The approvals state of a walk over several pages: any page that did not
 * include approvals keeps the walk from reading as included, and unavailable
 * (or an unknown state) wins over partial.
 */
export function mergeApprovalsSource(a: AttentionSourceState | undefined, b: AttentionSourceState | undefined): AttentionSourceState | undefined {
  const weight = (state: AttentionSourceState | undefined) => (state === undefined ? 3 : approvalsStateWeight[state] ?? 2)
  return weight(b) > weight(a) ? b : a
}

/**
 * What the response's optional sources leave out, kept apart from scan
 * limits: a skipped source is not a bound that was hit, so `incomplete` can
 * be false while approvals or monitor actions were never read. A missing
 * approvals state counts as unread.
 */
export function attentionSourceNotes(sources: Partial<AttentionResponse['sources']> | undefined): AttentionSourceNotes {
  const approvalsNote = approvalsNoteFor(sources?.approvals)
  const approvals = { approvalsUnread: approvalsNote !== undefined, approvalsPartial: sources?.approvals === 'partial', ...(approvalsNote ? { approvalsNote } : {}) }
  switch (sources?.monitors) {
    case 'forbidden':
      return { ...approvals, monitorsNote: 'Blocked monitor actions are not shown: your token cannot list repository monitors.' }
    case 'unavailable':
      return { ...approvals, monitorsNote: 'Blocked monitor actions are not shown: monitor storage is not enabled on this controller.' }
    default:
      return approvals
  }
}

/** The empty Needs you text, qualified when approvals were not all checked. */
export function needsYouEmptyText(notes: AttentionSourceNotes | undefined): string {
  if (notes?.approvalsPartial) return 'Nothing else needs you right now, but some approvals could not be checked'
  if (notes?.approvalsUnread) return 'Nothing else needs you right now, but approvals were not checked'
  return 'Nothing needs you right now'
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
