import type { Task } from '@/schemas/task'

const terminalPhases = new Set(['Succeeded', 'Failed', 'Cancelled'])

// Derived from the Task itself so list rows never need a per-row approvals
// request. The controller declares a WaitingForApproval condition, and an
// autonomous task parked on an approval reports "waiting for approval ..." as
// its status message.
export function isWaitingForApproval(task: Task): boolean {
  if (terminalPhases.has(task.status?.phase ?? '')) return false
  const condition = task.status?.conditions?.find((c) => c.type === 'WaitingForApproval')
  return condition?.status === 'True' || (task.status?.message?.startsWith('waiting for approval ') ?? false)
}

// The controller reports a Running Task whose Pod cannot start (an image
// pull failure, a missing config reference) as "pod waiting: <Reason> -
// <detail>" in its status message, and clears it once the Pod moves on.
const podWaitingPrefix = 'pod waiting: '

/** The status message of a Running Task whose Pod is stuck waiting, if any. */
export function podWaitingMessage(task: Task): string | undefined {
  const message = task.status?.message
  return task.status?.phase === 'Running' && message?.startsWith(podWaitingPrefix) ? message : undefined
}

/** A delete was accepted and finalizers are still running. */
export function isTaskDeleting(task: Task): boolean {
  return Boolean(task.metadata.deletionTimestamp)
}

/**
 * A Pending or Running task that a person can still cancel. A scheduled (cron)
 * Task is a template in every phase; it is suspended, never cancelled.
 */
export function isTaskCancellable(task: Task): boolean {
  const phase = task.status?.phase ?? ''
  return (
    !isTaskDeleting(task) && !task.spec.schedule?.trim() && (phase === '' || phase === 'Pending' || phase === 'Running')
  )
}

/** A task whose cancellation was requested but that has not ended yet. */
export function isTaskCancelling(task: Task): boolean {
  return isTaskCancellable(task) && task.metadata.annotations?.['orka.ai/cancel-requested'] !== undefined
}

/** A scheduled (cron) Task whose future runs are suspended. */
export function isScheduleSuspended(task: Task): boolean {
  return Boolean(task.spec.schedule) && task.spec.suspend === true
}

/**
 * The Task whose cancel dialog is open after a dialog for identity opens or
 * closes. A dialog closes only itself: a late close from a replaced Task's
 * dialog, whose request finished after the Task changed, leaves the
 * successor's dialog open.
 */
export function nextCancelTarget(current: string | null, identity: string, open: boolean): string | null {
  if (open) return identity
  return current === identity ? null : current
}
