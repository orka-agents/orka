import type { Task } from '@/schemas/task'
import { taskTypeLabel } from '@/lib/task-status'

export function matchesTaskFilter(task: Task, filter: string): boolean {
  const needle = filter.trim().toLowerCase()
  if (!needle) return true
  const name = task.metadata.name.toLowerCase()
  const tokens = [task.spec.type, task.status?.phase ?? '', taskTypeLabel(task.spec.type)]
    .filter(Boolean)
    .map((token) => token.toLowerCase())
  return needle.split(/\s+/).every((term) => name.includes(term) || tokens.some((token) => token.startsWith(term)))
}

export function compareTasksNewestFirst(a: Task, b: Task): number {
  const aTime = a.metadata.creationTimestamp ? Date.parse(a.metadata.creationTimestamp) : 0
  const bTime = b.metadata.creationTimestamp ? Date.parse(b.metadata.creationTimestamp) : 0
  return (Number.isNaN(bTime) ? 0 : bTime) - (Number.isNaN(aTime) ? 0 : aTime)
}
