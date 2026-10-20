import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { ShieldAlert } from 'lucide-react'
import { ActivitySpotlight } from '@/components/runtime/activity-spotlight'
import { RuntimeControlBar } from '@/components/runtime/runtime-control-bar'
import { ValidationSummary } from '@/components/runtime/validation-summary'
import { isLiveTask } from '@/lib/runtime-activity'
import type { Task } from '@/schemas/task'
import type { ExecutionEvent, TaskTrace, Approval } from '@/schemas/execution-event'
import type { ArtifactMetadata } from '@/schemas/artifact'
import { ExecutionGraph } from './execution-graph'
import { TaskExecutionPanel } from './task-execution-panel'

interface TaskOverviewProps {
  task: Task
  events?: ExecutionEvent[]
  trace?: TaskTrace
  approvals?: Approval[]
  artifacts?: ArtifactMetadata[]
  /** 'error' when execution events failed to load, 'unsupported' when the server has them off. */
  eventsStatus?: 'error' | 'unsupported'
  /** Owned by the parent that controls polling, so pause truly pauses. */
  following?: boolean
  onToggleFollow?: () => void
  /** False when execution-event storage is unavailable. */
  forkSupported?: boolean
  /** Response-level latest seq (0 for an empty stream); pins fork afterSeq. */
  latestSeq?: number
  pendingApprovals?: number
  onReviewApprovals?: () => void
}

/**
 * Where the task is and whether it needs anyone: what it is doing now, its
 * progress and delivery, derived checks, and its child tasks. The parent
 * fetches everything, so this is pure presentation; follow/pause is the
 * parent's so toggling it gates the parent's polling.
 */
export function TaskOverview({
  task,
  events = [],
  trace,
  approvals,
  artifacts,
  eventsStatus,
  following = true,
  onToggleFollow,
  forkSupported = true,
  latestSeq,
  pendingApprovals = 0,
  onReviewApprovals,
}: TaskOverviewProps) {
  const latestEvent = events[events.length - 1]
  // Prefer the response-level seq (0 for an empty stream) so fork afterSeq is
  // pinned even before any event arrives; fall back to the last event's seq.
  const pinnedSeq = latestSeq ?? latestEvent?.seq
  const hasChildren = (task.status?.childTasks?.length ?? 0) > 0
  return (
    <div className="space-y-4">
      <RuntimeControlBar task={task} following={following} onToggleFollow={onToggleFollow} latestSeq={pinnedSeq} forkSupported={forkSupported} />
      {pendingApprovals > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-status-running/40 bg-status-running-bg px-4 py-3 text-sm">
          <span className="flex items-center gap-2">
            <ShieldAlert className="h-4 w-4 shrink-0 text-status-running" aria-hidden="true" />
            {pendingApprovals === 1 ? '1 approval is' : `${pendingApprovals} approvals are`} waiting for a decision.
          </span>
          {onReviewApprovals && <Button size="sm" variant="outline" onClick={onReviewApprovals}>Review</Button>}
        </div>
      )}
      {/* The spotlight and token counts come from execution events. */}
      {eventsStatus === 'error' && (
        <p className="text-sm text-destructive" role="alert">Unable to load execution events; activity and token counts may be out of date.</p>
      )}
      {eventsStatus === 'unsupported' && (
        <p className="text-sm text-muted-foreground">Execution events are not enabled on this server, so activity and token counts are unavailable.</p>
      )}
      <div className="grid min-w-0 gap-4 lg:grid-cols-3">
        <div className="min-w-0 lg:col-span-2">
          <ActivitySpotlight task={task} latestEvent={latestEvent} following={isLiveTask(task) && following} />
        </div>
        <div className="min-w-0">
          <ValidationSummary task={task} trace={trace} approvals={approvals} artifacts={artifacts} />
        </div>
      </div>
      <TaskExecutionPanel task={task} events={events} />
      {hasChildren && (
        <Card>
          <CardHeader>
            <CardTitle>Child tasks</CardTitle>
          </CardHeader>
          <CardContent>
            <ExecutionGraph task={task} events={events} />
          </CardContent>
        </Card>
      )}
    </div>
  )
}
