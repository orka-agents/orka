import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { useTaskList } from '@/hooks/use-tasks'
import { ApiError } from '@/lib/api-client'
import { KanbanCard } from './kanban-card'
import type { Task, TaskPhase } from '@/schemas/task'

const columns: { phase: TaskPhase; label: string; color: string }[] = [
  { phase: 'Pending', label: 'Pending', color: 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-200' },
  { phase: 'Running', label: 'Running', color: 'bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200' },
  { phase: 'Succeeded', label: 'Succeeded', color: 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200' },
  { phase: 'Failed', label: 'Failed', color: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200' },
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
  const { data, isLoading, isError, error } = useTaskList('100', 5000)
  const tasks = data?.items ?? []
  const grouped = groupByPhase(tasks)

  if (isError) {
    const message = error instanceof ApiError && error.status === 401
      ? 'Unauthorized / token invalid'
      : 'Failed to load board'
    return (
      <div className="space-y-4">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Board</h1>
          <p className="text-muted-foreground">Kanban view of task execution</p>
        </div>
        <div className="rounded-md border border-destructive/40 bg-destructive/5 p-4 text-sm text-destructive" role="alert">
          {message}
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-3xl font-bold tracking-tight">Board</h1>
        <p className="text-muted-foreground">Kanban view of task execution</p>
      </div>
      <div className="flex gap-4 overflow-x-auto pb-4">
        {columns.map(({ phase, label, color }) => (
          <div key={phase} className="flex flex-col min-w-[280px] flex-1">
            <div className="flex items-center gap-2 mb-3 px-1">
              <h2 className="font-semibold text-sm">{label}</h2>
              <Badge className={color} variant="secondary">
                {isLoading ? '…' : grouped[phase].length}
              </Badge>
            </div>
            <div className="flex flex-col gap-2 min-h-[120px]">
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
        ))}
      </div>
    </div>
  )
}
