import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import type { Task } from '@/schemas/task'
import { isScheduleSuspended, isTaskDeleting, isWaitingForApproval } from './task-state'

/**
 * Compact state flags shown next to a task's phase wherever tasks are
 * listed: Deleting, Waiting for approval, and Suspended (schedule).
 */
export function TaskStateBadges({ task, className }: { task: Task; className?: string }) {
  const deleting = isTaskDeleting(task)
  const waiting = isWaitingForApproval(task)
  const suspended = isScheduleSuspended(task)
  if (!deleting && !waiting && !suspended) return null
  return (
    <span className={cn('inline-flex flex-wrap items-center gap-1', className)}>
      {deleting && (
        <Badge variant="outline" className="text-status-failed">Deleting</Badge>
      )}
      {waiting && (
        <Badge variant="outline" className="gap-1.5 text-status-running">
          <span aria-hidden="true" className="inline-block size-1.5 rounded-full bg-current motion-safe:animate-pulse-live" />
          Waiting for approval
        </Badge>
      )}
      {suspended && (
        <Badge variant="outline" className="text-muted-foreground">Suspended</Badge>
      )}
    </span>
  )
}
