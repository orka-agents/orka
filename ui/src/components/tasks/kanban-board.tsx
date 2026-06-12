import { Skeleton } from '@/components/ui/skeleton'
import { useTaskList } from '@/hooks/use-tasks'
import { phaseStyle } from '@/lib/task-status'
import { cn } from '@/lib/utils'
import { PageHeader } from '@/components/layout/page-header'
import { KanbanCard } from './kanban-card'
import type { Task, TaskPhase } from '@/schemas/task'

const columns: { phase: TaskPhase; label: string }[] = [
  { phase: 'Pending', label: 'Pending' },
  { phase: 'Running', label: 'Running' },
  { phase: 'Succeeded', label: 'Succeeded' },
  { phase: 'Failed', label: 'Failed' },
]

function groupByPhase(tasks: Task[]): Record<string, Task[]> {
  const groups: Record<string, Task[]> = { Pending: [], Running: [], Succeeded: [], Failed: [] }
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
  const { data, isLoading } = useTaskList('100', 5000)
  const tasks = data?.items ?? []
  const grouped = groupByPhase(tasks)

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
    </div>
  )
}
