import { Card, CardContent } from '@/components/ui/card'
import { ListAccessError } from '@/components/ui/list-access-error'
import { isForbiddenError } from '@/lib/api-client'
import { Skeleton } from '@/components/ui/skeleton'
import { useTaskListAll } from '@/hooks/use-tasks'
import { phaseStyle } from '@/lib/task-status'
import { cn } from '@/lib/utils'
import { PageHeader } from '@/components/layout/page-header'
import { KanbanCard } from './kanban-card'
import type { Task, TaskPhase } from '@/schemas/task'

// One column per backend task phase, in lifecycle order, so every task lands in
// its real column instead of being mis-bucketed as Pending.
const columns: { phase: TaskPhase; label: string }[] = [
  { phase: 'Pending', label: 'Pending' },
  { phase: 'Scheduled', label: 'Scheduled' },
  { phase: 'Running', label: 'Running' },
  { phase: 'Finalizing', label: 'Finalizing' },
  { phase: 'Succeeded', label: 'Succeeded' },
  { phase: 'Failed', label: 'Failed' },
  { phase: 'Cancelled', label: 'Cancelled' },
]

function groupByPhase(tasks: Task[]): Record<string, Task[]> {
  const groups: Record<string, Task[]> = Object.fromEntries(
    columns.map((c) => [c.phase, [] as Task[]]),
  )
  for (const task of tasks) {
    const phase = task.status?.phase ?? 'Pending'
    if (groups[phase]) {
      groups[phase].push(task)
    } else {
      groups.Pending.push(task)
    }
  }
  return groups
}

export function KanbanBoard() {
  // Walk every page up to the shared summary cap (like the Dashboard) so
  // column counts cover the namespace instead of the first 100 names; the
  // walk is heavier than one page, so it polls at the Live view's cadence.
  const { data, isLoading, error: listError } = useTaskListAll('100', 10000)
  // A 403 always replaces the board; any other failure only blocks a board
  // that never loaded, so a failed background refresh keeps the last one.
  const error = listError && (isForbiddenError(listError) || !data) ? listError : null
  const tasks = data?.items ?? []
  const truncated = Boolean(data?.truncated)
  const grouped = groupByPhase(tasks)

  return (
    <div className="space-y-4">
      <PageHeader title="Board" description="Kanban view of task execution" />
      {truncated && !error && (
        <p className="text-sm text-muted-foreground" role="status">
          Column counts use {tasks.length.toLocaleString()} loaded tasks in resource-key order. More tasks exist.
        </p>
      )}
      {/* A failed walk (403 or server error) leaves nothing to bucket; say so
          instead of rendering every column as an empty "No ... tasks". */}
      {error ? (
        <Card><CardContent><ListAccessError error={error} resource="tasks" /></CardContent></Card>
      ) : (
      <div className="flex gap-4 overflow-x-auto pb-4">
        {columns.map(({ phase, label }) => {
          const style = phaseStyle(phase)
          return (
          <div
            key={phase}
            className={cn(
              'flex flex-col min-w-[280px] flex-1 rounded-lg border-t-2 px-1 pt-2',
              style.railClass,
              style.bgClass,
            )}
          >
            <div className="flex items-center gap-2 mb-3 px-1">
              {style.live && (
                <span
                  data-testid="live-indicator"
                  className="inline-block size-2 rounded-full bg-live motion-safe:animate-pulse-live"
                  aria-hidden="true"
                />
              )}
              <h2 className="font-semibold text-sm">{label}</h2>
              <span
                className={cn(
                  // On the card surface, not the column's own tint, so the
                  // status color keeps AA contrast.
                  'inline-flex items-center justify-center rounded-full border bg-card px-2 py-0.5 text-xs font-medium tabular-nums',
                  style.textClass,
                )}
              >
                {isLoading ? '…' : grouped[phase].length}
              </span>
            </div>
            <div className="flex flex-col gap-2 min-h-[120px] pb-2">
              {isLoading ? (
                Array.from({ length: 2 }).map((_, i) => (
                  <Skeleton key={i} className="h-20 w-full rounded-xl" />
                ))
              ) : grouped[phase].length === 0 ? (
                <p className="text-xs text-muted-foreground text-center py-8">No {label.toLowerCase()} tasks</p>
              ) : (
                grouped[phase].map((task) => (
                  <KanbanCard key={task.metadata.uid || task.metadata.name} task={task} />
                ))
              )}
            </div>
          </div>
          )
        })}
      </div>
      )}
    </div>
  )
}
