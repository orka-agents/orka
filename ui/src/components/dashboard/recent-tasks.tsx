import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import type { Task } from '@/schemas/task'

const phaseColors: Record<string, string> = {
  Pending: 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-200',
  Running: 'bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200',
  Succeeded: 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200',
  Failed: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
}

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
          <p className="text-sm text-muted-foreground">No tasks yet</p>
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
                  <p className="text-xs text-muted-foreground">
                    {task.spec.type} · {task.metadata.namespace} · {timeAgo(task.metadata.creationTimestamp)}
                  </p>
                </div>
                <Badge className={phaseColors[task.status?.phase ?? 'Pending']} variant="secondary">
                  {task.status?.phase ?? 'Pending'}
                </Badge>
              </Link>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}
