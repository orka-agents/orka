import { Link } from '@tanstack/react-router'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { TaskStatusBadge } from './task-status-badge'
import { useChildTasks } from '@/hooks/use-child-tasks'
import type { Task } from '@/schemas/task'

function ChildTaskRow({ task, isLast }: { task: Task; isLast: boolean }) {
  const prefix = isLast ? '└' : '├'
  return (
    <div className="flex items-center gap-2 py-1 text-sm">
      <span className="font-mono text-muted-foreground w-4 text-center">{prefix}</span>
      <Link to="/tasks/$taskId" params={{ taskId: task.metadata.name }} className="font-medium text-primary hover:underline">
        {task.metadata.name}
      </Link>
      {task.spec.agentRef?.name && (
        <Badge variant="outline">{task.spec.agentRef.name}</Badge>
      )}
      <TaskStatusBadge phase={task.status?.phase} />
      {task.status?.message && (
        <span className="text-muted-foreground truncate max-w-[200px]" title={task.status.message}>
          {task.status.message}
        </span>
      )}
    </div>
  )
}

export function ChildTaskTree({ parentTaskName }: { parentTaskName: string }) {
  const { data, isLoading } = useChildTasks(parentTaskName)

  if (isLoading) {
    return (
      <div className="space-y-2" data-testid="child-task-loading">
        <Skeleton className="h-6 w-full" />
        <Skeleton className="h-6 w-full" />
      </div>
    )
  }

  const children = data?.items ?? []

  if (children.length === 0) {
    return <p className="text-sm text-muted-foreground">No child tasks</p>
  }

  return (
    <div className="space-y-0.5">
      {children.map((task, i) => (
        <ChildTaskRow key={task.metadata.name} task={task} isLast={i === children.length - 1} />
      ))}
    </div>
  )
}
