import type { Task } from '@/schemas/task'
import { taskTypeLabel } from '@/lib/task-status'

// matchesTaskFilter is a client-side name/type/phase search over every
// loaded task, not just the rows revealed so far. Each term is a substring of
// the name or the start of the type or phase: matching inside a word made
// "ai" match every Container task.
export function matchesTaskFilter(task: Task, filter: string): boolean {
  const needle = filter.trim().toLowerCase()
  if (!needle) return true
  const name = task.metadata.name.toLowerCase()
  const tokens = [task.spec.type, task.status?.phase ?? '', taskTypeLabel(task.spec.type)]
    .filter(Boolean)
    .map((token) => token.toLowerCase())
  return needle.split(/\s+/).every((term) => name.includes(term) || tokens.some((token) => token.startsWith(term)))
}

/** Newest first by creationTimestamp; the API pages in name order. */
export function compareTasksNewestFirst(a: Task, b: Task): number {
  const aTime = a.metadata.creationTimestamp ? Date.parse(a.metadata.creationTimestamp) : 0
  const bTime = b.metadata.creationTimestamp ? Date.parse(b.metadata.creationTimestamp) : 0
  return (Number.isNaN(bTime) ? 0 : bTime) - (Number.isNaN(aTime) ? 0 : aTime)
}
