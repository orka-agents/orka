import { Skeleton } from '@/components/ui/skeleton'
import { useTaskListAll } from '@/hooks/use-tasks'
import { phaseStyle } from '@/lib/task-status'
import { cn } from '@/lib/utils'
import { PageHeader } from '@/components/layout/page-header'
import { KanbanCard } from './kanban-card'
import { TaskInventoryError } from './task-inventory-error'
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
  const { data, error, isFetching, refetch } = useTaskListAll('100', 5000)

  if (error) {
    return (
      <div className="space-y-4">
        <PageHeader title="Board" description="Kanban view of task execution" />
        <TaskInventoryError
          isRetrying={isFetching}
          onRetry={() => void refetch()}
        />
      </div>
    )
  }

  if (!data) {
    return (
      <div className="space-y-4">
        <PageHeader title="Board" description="Kanban view of task execution" />
        <div
          role="status"
          aria-label="Loading complete task inventory"
          className="flex gap-4 overflow-x-auto pb-4"
        >
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-40 min-w-[280px] flex-1" />
          ))}
        </div>
      </div>
    )
  }

  const grouped = groupByPhase(data.items)

  return (
    <div className="space-y-4">
      <PageHeader title="Board" description="Kanban view of task execution" />
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
                  'inline-flex items-center justify-center rounded-full px-2 py-0.5 text-xs font-medium tabular-nums',
                  style.bgClass,
                  style.textClass,
                )}
              >
                {grouped[phase].length}
              </span>
            </div>
            <div className="flex flex-col gap-2 min-h-[120px] pb-2">
              {grouped[phase].length === 0 ? (
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
    </div>
  )
}
