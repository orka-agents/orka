import { useEffect, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { AlertTriangle } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { SonarPing } from '@/components/ui/sonar-ping'
import { StatusDot } from '@/components/ui/status-dot'
import { useFreshness } from '@/hooks/use-freshness'
import { cn } from '@/lib/utils'
import { elapsedSeconds, formatElapsed, UNASSIGNED_AGENT } from '@/lib/runtime-activity'
import { isTaskTerminal } from '@/lib/task-phase'
import { isWaitingForApproval, podWaitingMessage } from '@/components/tasks/task-state'
import { TaskStateBadges } from '@/components/tasks/task-state-badges'
import type { Task } from '@/schemas/task'
import type { ExecutionEvent } from '@/schemas/execution-event'

interface ActivitySpotlightProps {
  task: Task | null
  latestEvent?: ExecutionEvent
  following: boolean
  eventError?: boolean
}

export function ActivitySpotlight({ task, latestEvent, following, eventError = false }: ActivitySpotlightProps) {
  const [now, setNow] = useState(() => Date.now())
  const phase = task?.status?.phase
  const running = phase === 'Running'
  const waitingForApproval = task ? isWaitingForApproval(task) : false
  const podWaiting = task ? podWaitingMessage(task) : undefined
  const headline = running && !waitingForApproval && !podWaiting
    ? latestEvent?.summary ?? task?.status?.message
    : task?.status?.message ?? latestEvent?.summary
  const justUpdated = useFreshness(headline)

  useEffect(() => {
    if (!task || phase !== 'Running' || elapsedSeconds(task, now) === null) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [task])

  if (!task) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center gap-3 py-8">
          <SonarPing />
          <p className="text-sm font-medium">No active task</p>
          <p className="text-xs text-muted-foreground">
            Running agents in this namespace will spotlight here as they start.
          </p>
        </CardContent>
      </Card>
    )
  }

  const agent = task.spec.agentRef?.name || UNASSIGNED_AGENT
  const terminal = isTaskTerminal(phase)
  const endMs = terminal && task.status?.completionTime ? new Date(task.status.completionTime).getTime() : now
  const elapsed = formatElapsed(running || terminal ? elapsedSeconds(task, endMs) : null)
  const heading = terminal ? 'Last run' : running ? 'Active now' : 'Waiting'
  const liveLabel = terminal ? 'Completed' : running ? (following ? 'Following live' : 'Paused') : 'Not running'

  return (
    <Card className={cn('border-l-2', running ? 'border-l-live' : 'border-l-transparent', justUpdated && 'motion-safe:animate-freshness')}>
      <CardContent className="space-y-2 py-5">
        <div className="flex items-center justify-between gap-3">
          <p className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
            {heading}
          </p>
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground" aria-live="polite">
            <span
              aria-hidden="true"
              className={cn(
                'inline-block size-2 shrink-0 rounded-full',
                running && following ? 'bg-status-running motion-safe:animate-pulse-live' : 'bg-muted-foreground',
              )}
            />
            {liveLabel}
          </span>
        </div>
        <div className="flex items-center justify-between gap-3">
          <Link
            to="/tasks/$taskId"
            params={{ taskId: task.metadata.name }}
            title={task.metadata.name}
            className="truncate font-mono text-lg font-semibold hover:underline"
          >
            {task.metadata.name}
          </Link>
          <StatusDot phase={task.status?.phase} />
        </div>
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
          <span className="min-w-0 max-w-full truncate" title={agent}>{agent}</span>
          <span aria-hidden="true">·</span>
          <span className="tabular-nums">{elapsed}</span>
          <TaskStateBadges task={task} />
        </div>
        {eventError && (
          <p className="text-sm text-destructive" role="alert">Unable to load events</p>
        )}
        {podWaiting ? (
          <p className="flex items-start gap-1.5 break-words text-sm text-status-pending" role="status">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span className="min-w-0">{podWaiting}</span>
          </p>
        ) : (
          headline && <p className="truncate text-sm text-muted-foreground">{headline}</p>
        )}
      </CardContent>
    </Card>
  )
}
