import { Link } from '@tanstack/react-router'
import { CalendarClock } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { useTaskChildren } from '@/hooks/use-tasks'
import { formatTimestamp, humanizeDuration, timeAgo } from '@/lib/time'
import type { Task } from '@/schemas/task'
import { TaskStatusBadge } from './task-status-badge'

const recentRunLimit = 10

function untilLabel(ts?: string): string {
  if (!ts) return ''
  const ms = new Date(ts).getTime()
  if (Number.isNaN(ms)) return ''
  const seconds = (ms - Date.now()) / 1000
  return seconds > 0 ? ` (in ${humanizeDuration(seconds)})` : ' (due)'
}

// A run belongs to this schedule when the controller made this exact Task
// (by UID) its owner; a same-name recreation must not list older runs.
function isRunOf(child: Task, parent: Task): boolean {
  const owners = child.metadata.ownerReferences ?? []
  if (parent.metadata.uid && owners.length > 0) {
    return owners.some((owner) => owner.kind === 'Task' && owner.uid === parent.metadata.uid)
  }
  return child.metadata.labels?.['orka.ai/scheduled-run'] === 'true'
}

/**
 * Schedule state for a cron Task: the expression, suspension, last and next
 * run, and links to the recent runs it created.
 */
export function TaskSchedulePanel({ task }: { task: Task }) {
  const schedule = task.spec.schedule
  const { data, isLoading, error } = useTaskChildren(task.metadata.name, Boolean(schedule), task.metadata.uid)
  if (!schedule) return null
  const suspended = task.spec.suspend === true
  const runs = (data?.items ?? [])
    .filter((child) => isRunOf(child, task))
    .sort((a, b) => Date.parse(b.metadata.creationTimestamp ?? '') - Date.parse(a.metadata.creationTimestamp ?? ''))
    .slice(0, recentRunLimit)

  return (
    <Card aria-label="Schedule">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <CalendarClock className="size-4" aria-hidden="true" /> Schedule
          {suspended && <Badge variant="outline" className="text-muted-foreground">Suspended</Badge>}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        <div className="grid gap-2 md:grid-cols-2">
          <div>
            <span className="text-muted-foreground">Cron:</span>{' '}
            <code className="rounded bg-muted px-1 font-mono">{schedule}</code>{' '}
            <span className="text-muted-foreground">{task.spec.timeZone || 'UTC'}</span>
          </div>
          <div>
            <span className="text-muted-foreground">Concurrency:</span> {task.spec.concurrencyPolicy || 'Forbid'}
          </div>
          <div>
            <span className="text-muted-foreground">Last run:</span>{' '}
            <span className="tabular-nums">
              {task.status?.lastScheduleTime
                ? `${formatTimestamp(task.status.lastScheduleTime)} (${timeAgo(task.status.lastScheduleTime)})`
                : 'Never'}
            </span>
          </div>
          <div>
            <span className="text-muted-foreground">Next run:</span>{' '}
            <span className="tabular-nums">
              {suspended
                ? 'Suspended'
                : task.status?.nextScheduleTime
                  ? `${formatTimestamp(task.status.nextScheduleTime)}${untilLabel(task.status.nextScheduleTime)}`
                  : 'Not scheduled yet'}
            </span>
          </div>
        </div>
        <div className="space-y-2">
          <p className="font-medium">Recent runs</p>
          {isLoading ? (
            <p className="text-muted-foreground">Loading runs…</p>
          ) : error ? (
            <p className="text-muted-foreground" role="alert">Could not load runs ({error instanceof Error ? error.message : 'unknown error'}).</p>
          ) : runs.length === 0 ? (
            <p className="text-muted-foreground">No runs retained yet.</p>
          ) : (
            <ul className="space-y-1">
              {runs.map((run) => (
                <li key={run.metadata.uid || run.metadata.name} className="flex flex-wrap items-center gap-2">
                  <Link
                    to="/tasks/$taskId"
                    params={{ taskId: run.metadata.name }}
                    className="font-mono text-xs hover:underline"
                  >
                    {run.metadata.name}
                  </Link>
                  <TaskStatusBadge phase={run.status?.phase} />
                  <span className="text-xs text-muted-foreground tabular-nums">{timeAgo(run.metadata.creationTimestamp)}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </CardContent>
    </Card>
  )
}
