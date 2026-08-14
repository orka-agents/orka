import { CalendarClock } from 'lucide-react'

import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import type { Task } from '@/schemas/task'

function Item({ label, value }: { label: string; value?: string | number | null }) {
  if (value === undefined || value === null || value === '') return null
  return (
    <div>
      <span className="text-muted-foreground">{label}:</span>{' '}
      <span className="font-mono text-xs">{value}</span>
    </div>
  )
}

// Cron facts for scheduled tasks: the parent stays in phase Scheduled while
// each tick mints a child run.
export function TaskScheduleCard({ task }: { task: Task }) {
  if (!task.spec.schedule) return null

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <CalendarClock className="h-4 w-4" />
          Schedule
          {task.spec.suspend && (
            <Badge variant="secondary" className="bg-status-pending-bg text-status-pending">Suspended</Badge>
          )}
        </CardTitle>
      </CardHeader>
      <CardContent className="grid gap-2 text-sm md:grid-cols-2">
        <Item label="Cron" value={task.spec.schedule} />
        <Item label="Time zone" value={task.spec.timeZone ?? 'UTC'} />
        <Item label="Concurrency" value={task.spec.concurrencyPolicy ?? 'Forbid'} />
        <Item label="Starting deadline" value={task.spec.startingDeadlineSeconds !== undefined ? `${task.spec.startingDeadlineSeconds}s` : undefined} />
        <Item label="Last run" value={task.status?.lastScheduleTime ? new Date(task.status.lastScheduleTime).toLocaleString() : undefined} />
        <Item label="Next run" value={task.status?.nextScheduleTime ? new Date(task.status.nextScheduleTime).toLocaleString() : undefined} />
        <Item label="Keep successful runs" value={task.spec.successfulRunsHistoryLimit} />
        <Item label="Keep failed runs" value={task.spec.failedRunsHistoryLimit} />
      </CardContent>
    </Card>
  )
}
