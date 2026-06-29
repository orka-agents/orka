import { useEffect, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Card, CardContent } from '@/components/ui/card'
import { SonarPing } from '@/components/ui/sonar-ping'
import { StatusDot } from '@/components/ui/status-dot'
import { useFreshness } from '@/hooks/use-freshness'
import { cn } from '@/lib/utils'
import { elapsedSeconds, formatElapsed, UNASSIGNED_AGENT } from '@/lib/runtime-activity'
import { isTerminal } from '@/lib/runtime-validation'
import type { Task } from '@/schemas/task'
import type { ExecutionEvent } from '@/schemas/execution-event'

interface ActivitySpotlightProps {
  /** The current active task, or null when nothing is running. */
  task: Task | null
  /** Latest execution event for the active task, if known. */
  latestEvent?: ExecutionEvent
  /** True while a live stream/poll is following the namespace. */
  following: boolean
  /** True when the focused event stream failed and the headline may be stale. */
  eventError?: boolean
}

/**
 * Hero panel: who is active, what they're doing, how long, and whether we're
 * live. Idle (no running task) shows a clear, non-misleading resting state.
 */
export function ActivitySpotlight({ task, latestEvent, following, eventError = false }: ActivitySpotlightProps) {
  const [now, setNow] = useState(() => Date.now())
  const phase = task?.status?.phase
  const running = phase === 'Running'
  const headline = running
    ? latestEvent?.summary ?? task?.status?.message
    : task?.status?.message ?? latestEvent?.summary
  const justUpdated = useFreshness(headline)

  useEffect(() => {
    // Only tick for a running task with a clock: terminal tasks freeze at
    // completionTime, waiting tasks are not active, and startTime-less tasks render "—".
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
  // For terminal tasks freeze the clock at completionTime (the run's duration);
  // a running task ticks against now. Pending/Scheduled tasks are waiting, not
  // active, even when opened in the task detail Runtime tab.
  const terminal = isTerminal(phase)
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
            className="truncate font-mono text-lg font-semibold hover:underline"
          >
            {task.metadata.name}
          </Link>
          <StatusDot phase={task.status?.phase} />
        </div>
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <span>{agent}</span>
          <span aria-hidden="true">·</span>
          <span className="tabular-nums">{elapsed}</span>
        </div>
        {eventError && (
          <p className="text-sm text-destructive" role="alert">Unable to load events</p>
        )}
        {headline && <p className="truncate text-sm text-muted-foreground">{headline}</p>}
      </CardContent>
    </Card>
  )
}
