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

export function isAgentWorking(task: Task): boolean {
  if (workingPhases.has(task.status?.phase ?? '')) return true
  if (task.status?.conditions?.some((c) => c.type === 'WaitingForCapacity' && c.status === 'True')) return true
  const wait = task.status?.wait
  if ((wait?.kind !== 'Tasks' && wait?.kind !== 'Timer') || !activeWaitStates.has(wait.state ?? '')) return false
  return wait.state !== 'Requested' || task.status?.phase === 'Succeeded'
}

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

export const taskApprovalWaitSummary = 'Waiting for approval'

const taskApprovalWaitIdPrefix = 'task-approval-wait:'

export function isTaskDerivedItem(item: AttentionItem): boolean {
  return item.id.startsWith(taskApprovalWaitIdPrefix)
}

export function isTaskAwaitingApproval(task: Task): boolean {
  return !isTaskDeleting(task) && isWaitingForApproval(task)
}

function taskApprovalWaitItem(task: Task): AttentionItem {
  const condition = task.status?.conditions?.find((c) => c.type === 'WaitingForApproval' && c.status === 'True')
  return {
    id: `${taskApprovalWaitIdPrefix}${taskKey(task)}`,
    kind: 'approval',
    rank: 1,
    since: condition?.lastTransitionTime ?? task.metadata.creationTimestamp ?? '',
    summary: taskApprovalWaitSummary,
    task: { namespace: task.metadata.namespace ?? '', name: task.metadata.name, uid: task.metadata.uid ?? '', phase: task.status?.phase },
  }
}

export function groupByAttention(tasks: Task[], items: AttentionItem[]): { needsYou: AttentionItem[]; working: Task[]; done: Task[] } {
  const flagged = attentionTaskKeys(items)
  const approvalFlagged = attentionTaskKeys(items.filter((item) => item.kind === 'approval'))
  const derived: AttentionItem[] = []
  const working: Task[] = []
  const done: Task[] = []
  for (const task of tasks) {
    const key = taskKey(task)
    const awaitingApproval = isTaskAwaitingApproval(task)
    if (awaitingApproval && !approvalFlagged.has(key)) derived.push(taskApprovalWaitItem(task))
    if (flagged.has(key) || awaitingApproval) continue
    if (isAgentWorking(task)) working.push(task)
    else done.push(task)
  }
  if (derived.length === 0) return { needsYou: items, working, done }
  const needsYou = [...items, ...derived].sort((a, b) => a.rank - b.rank)
  return { needsYou, working, done }
}

const incompleteReasonText: Record<string, string> = {
  taskScanLimit: 'only the first Tasks in the namespace were scanned',
  approvalCandidateLimit: 'too many Tasks were waiting for approval to read them all',
  approvalEventLimit: 'a Task had too much approval history to read',
  monitorActionLimit: 'too many blocked monitor actions to read them all',
}

export function incompleteText(reasons: string[] | undefined): string {
  const parts = (reasons ?? []).map((reason) => incompleteReasonText[reason] ?? reason)
  return parts.length > 0 ? `This list may be incomplete: ${parts.join('; ')}.` : 'This list may be incomplete.'
}

export interface AttentionSourceNotes {
  approvalsUnread: boolean
  approvalsPartial: boolean
  approvalsNote?: string
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

const approvalsStateWeight: Partial<Record<AttentionSourceState, number>> = { included: 0, partial: 1 }

export function mergeApprovalsSource(a: AttentionSourceState | undefined, b: AttentionSourceState | undefined): AttentionSourceState | undefined {
  const weight = (state: AttentionSourceState | undefined) => (state === undefined ? 3 : approvalsStateWeight[state] ?? 2)
  return weight(b) > weight(a) ? b : a
}

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

export function deadlineText(deadline?: string, now = Date.now()): string | undefined {
  if (!deadline) return undefined
  const ms = new Date(deadline).getTime()
  if (Number.isNaN(ms)) return undefined
  const remaining = (ms - now) / 1000
  return remaining <= 0 ? 'overdue' : `due in ${humanizeDuration(remaining)}`
}

export type KanbanView = 'phase' | 'attention'

export function parseKanbanView(value: unknown): KanbanView {
  return value === 'attention' ? 'attention' : 'phase'
}
