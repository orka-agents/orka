import { useState, useEffect } from 'react'
import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { useTaskList } from '@/hooks/use-tasks'
import { TaskStatusBadge } from './task-status-badge'
import type { Task } from '@/schemas/task'

function AgentMiniPanel({ task }: { task: Task }) {
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [])

  const elapsed = task.status?.startTime
    ? Math.floor((now - new Date(task.status.startTime).getTime()) / 1000)
    : 0

  const formatElapsed = (s: number) => {
    if (s < 60) return `${s}s`
    if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
    return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
  }

  return (
    <Link to="/tasks/$taskId" params={{ taskId: task.metadata.name }}>
      <Card className="hover:border-primary/50 transition-colors cursor-pointer h-full">
        <CardHeader className="pb-2">
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm font-medium truncate">{task.metadata.name}</CardTitle>
            <TaskStatusBadge phase={task.status?.phase} />
          </div>
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            {task.spec.agentRef?.name && <span>{task.spec.agentRef.name}</span>}
            <span>{formatElapsed(elapsed)}</span>
          </div>
        </CardHeader>
        <CardContent className="pt-0">
          {task.status?.message && (
            <p className="text-xs text-muted-foreground truncate">{task.status.message}</p>
          )}
        </CardContent>
      </Card>
    </Link>
  )
}

export function AgentGridView() {
  const { data, isLoading } = useTaskList()

  const runningTasks = (data?.items ?? []).filter(t => t.status?.phase === 'Running')

  if (isLoading) {
    return (
      <div className="space-y-4">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Live Agents</h1>
          <p className="text-muted-foreground">Active task execution overview</p>
        </div>
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-32 w-full" />
          ))}
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-3xl font-bold tracking-tight">Live Agents</h1>
        <p className="text-muted-foreground">
          {runningTasks.length} active {runningTasks.length === 1 ? 'task' : 'tasks'}
        </p>
      </div>
      {runningTasks.length === 0 ? (
        <Card>
          <CardContent className="py-8 text-center text-muted-foreground">
            No tasks currently running
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {runningTasks.map(task => (
            <AgentMiniPanel key={task.metadata.uid || task.metadata.name} task={task} />
          ))}
        </div>
      )}
    </div>
  )
}
