import type { Task } from '@/schemas/task'

export function activeParkedApprovalId(task: Pick<Task, 'metadata' | 'status'>, now: number = Date.now()): string | undefined {
  const wait = task.status?.wait
  if (task.metadata.deletionTimestamp || wait?.kind !== 'Approval') return undefined
  if (wait.state === 'Requested' && task.status?.phase !== 'Succeeded') return undefined
  if (wait.state !== 'Requested' && wait.state !== 'Waiting') return undefined
  if (wait.deadline) {
    const deadline = Date.parse(wait.deadline)
    if (!Number.isNaN(deadline) && now >= deadline) return undefined
  }
  return wait.references?.length === 1 ? wait.references[0] : undefined
}
