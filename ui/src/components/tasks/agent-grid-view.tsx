import { useState, useEffect } from 'react'
import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { EmptyState } from '@/components/ui/empty-state'
import { SonarPing } from '@/components/ui/sonar-ping'
import { useTaskList } from '@/hooks/use-tasks'
import { useFreshness } from '@/hooks/use-freshness'
import { cn } from '@/lib/utils'
import { TaskStatusBadge } from './task-status-badge'
import type { Task } from '@/schemas/task'

function AgentMiniPanel({ task }: { task: Task }) {
  const [now, setNow] = useState(() => Date.now())
  // Glow briefly when this running task's status message changes — draws the
  // eye to what just advanced in a grid that polls every few seconds.
  const justUpdated = useFreshness(task.status?.message)

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
      <Card
        className={cn(
          'border-l-2 border-l-live hover:border-primary/50 hover:shadow-md transition-all motion-safe:hover:-translate-y-0.5 cursor-pointer h-full',
          justUpdated && 'motion-safe:animate-freshness',
        )}
      >
        <CardHeader className="pb-2">
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm font-medium truncate">{task.metadata.name}</CardTitle>
            <TaskStatusBadge phase={task.status?.phase} />
          </div>
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            {task.spec.agentRef?.name && <span>{task.spec.agentRef.name}</span>}
            <span className="tabular-nums">{formatElapsed(elapsed)}</span>
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
        <PageHeader title="Live Agents" description="Active task execution overview" />
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
      <PageHeader
        title="Live Agents"
        description={`${runningTasks.length} active ${runningTasks.length === 1 ? 'task' : 'tasks'}`}
      />
      {runningTasks.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-4 py-10">
            <SonarPing />
            <EmptyState
              headline="No tasks currently running"
              hint="Running agents will appear here as they start."
              className="py-0"
            />
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
