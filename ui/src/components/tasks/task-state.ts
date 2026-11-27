import type { Task } from '@/schemas/task'
import { isTaskTerminal } from '@/lib/task-phase'
import { activeParkedApprovalId } from '@/lib/parked-approval'

export function isWaitingForApproval(task: Task): boolean {
  if (activeParkedApprovalId(task) !== undefined) return true
  if (isTaskTerminal(task.status?.phase)) return false
  const condition = task.status?.conditions?.find((c) => c.type === 'WaitingForApproval')
  return condition?.status === 'True' || (task.status?.message?.startsWith('waiting for approval ') ?? false)
}

const podWaitingPrefix = 'pod waiting: '

export function podWaitingMessage(task: Task): string | undefined {
  const message = task.status?.message
  return task.status?.phase === 'Running' && message?.startsWith(podWaitingPrefix) ? message : undefined
}

export function isTaskDeleting(task: Task): boolean {
  return Boolean(task.metadata.deletionTimestamp)
}

export function isTaskCancellable(task: Task): boolean {
  const phase = task.status?.phase ?? ''
  return (
    !isTaskDeleting(task) && !task.spec.schedule?.trim() && (phase === '' || phase === 'Pending' || phase === 'Running')
  )
}

export function isTaskCancelling(task: Task): boolean {
  return isTaskCancellable(task) && task.metadata.annotations?.['orka.ai/cancel-requested'] !== undefined
}

export function isScheduleSuspended(task: Task): boolean {
  return Boolean(task.spec.schedule) && task.spec.suspend === true
}

export function nextCancelTarget(current: string | null, identity: string, open: boolean): string | null {
  if (open) return identity
  return current === identity ? null : current
}
