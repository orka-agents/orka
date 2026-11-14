import type { Task } from '@/schemas/task'

/**
 * The sign-off approval a parked turn is still waiting on. The task's own turn
 * has ended, so its phase is terminal, but the backend accepts a decision on
 * exactly this approval while the wait is active.
 */
export function activeParkedApprovalId(task: Pick<Task, 'metadata' | 'status'>): string | undefined {
  const wait = task.status?.wait
  if (task.metadata.deletionTimestamp || wait?.kind !== 'Approval') return undefined
  // A requested wait arms only after its turn succeeds; after a failed or
  // cancelled turn it is abandoned and takes no decision.
  if (wait.state === 'Requested' && task.status?.phase !== 'Succeeded') return undefined
  if (wait.state !== 'Requested' && wait.state !== 'Waiting') return undefined
  return wait.references?.length === 1 ? wait.references[0] : undefined
}
