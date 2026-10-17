import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { StatusDot } from '@/components/ui/status-dot'
import { EmptyState } from '@/components/ui/empty-state'
import { ListTodo } from 'lucide-react'
import type { Task } from '@/schemas/task'
import { timeAgo } from '@/lib/time'
import { TaskStateBadges } from '@/components/tasks/task-state-badges'
import { loadProblemText, type LoadProblem } from '@/lib/load-problem'

export function RecentTasks({
  tasks,
  isLoading,
  problem,
  isTruncated,
}: {
  tasks?: Task[]
  isLoading?: boolean
  problem?: LoadProblem
  isTruncated?: boolean
}) {
  if (problem) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Recent Tasks</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground" role="alert">
            {loadProblemText('tasks', problem)}
          </p>
        </CardContent>
      </Card>
    )
  }
  if (isLoading) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Recent Tasks</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="flex items-center justify-between">
              <Skeleton className="h-4 w-48" />
              <Skeleton className="h-5 w-20" />
            </div>
          ))}
        </CardContent>
      </Card>
    )
  }

  const recent = [...(tasks ?? [])]
    .sort((a, b) => {
      const aTime = a.metadata.creationTimestamp ? Date.parse(a.metadata.creationTimestamp) : 0
      const bTime = b.metadata.creationTimestamp ? Date.parse(b.metadata.creationTimestamp) : 0
      return bTime - aTime
    })
    .slice(0, 10)

  return (
    <Card>
      <CardHeader>
        <CardTitle>{isTruncated ? 'Task sample' : 'Recent Tasks'}</CardTitle>
        {isTruncated && (
          <p className="text-xs text-muted-foreground">
            Sorted by timestamp within the loaded sample. Newer tasks may exist outside it.
          </p>
        )}
      </CardHeader>
      <CardContent>
        {recent.length === 0 ? (
          <EmptyState
            icon={ListTodo}
            headline="No tasks yet"
            hint="Tasks you create will show up here."
          />
        ) : (
          <div className="space-y-3">
            {recent.map((task) => (
              <Link
                key={task.metadata.uid || task.metadata.name}
                to="/tasks/$taskId"
                params={{ taskId: task.metadata.name }}
                className="flex items-center justify-between rounded-md p-2 hover:bg-accent"
              >
                <div className="min-w-0 space-y-1">
                  <p className="flex flex-wrap items-center gap-2 text-sm font-medium">
                    <span>{task.metadata.name}</span>
                    <TaskStateBadges task={task} />
                  </p>
                  <p className="text-xs text-muted-foreground tabular-nums">
                    {task.spec.type} · {task.metadata.namespace} · {timeAgo(task.metadata.creationTimestamp)}
                  </p>
                </div>
                <StatusDot phase={task.status?.phase} />
              </Link>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}
