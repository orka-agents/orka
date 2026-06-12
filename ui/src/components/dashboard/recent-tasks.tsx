import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { StatusDot } from '@/components/ui/status-dot'
import { EmptyState } from '@/components/ui/empty-state'
import { ListTodo } from 'lucide-react'
import type { Task } from '@/schemas/task'

function timeAgo(timestamp?: string): string {
  if (!timestamp) return '-'
  const seconds = Math.floor((Date.now() - new Date(timestamp).getTime()) / 1000)
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

export function RecentTasks({ tasks, isLoading }: { tasks?: Task[]; isLoading?: boolean }) {
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

  const recent = (tasks ?? []).slice(0, 10)

  return (
    <Card>
      <CardHeader>
        <CardTitle>Recent Tasks</CardTitle>
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
                <div className="space-y-1">
                  <p className="text-sm font-medium">{task.metadata.name}</p>
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
