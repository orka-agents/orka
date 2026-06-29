import { ActivitySpotlight } from './activity-spotlight'
import { TaskFlowPanel } from './task-flow-panel'
import { ValidationSummary } from './validation-summary'
import { LiveStatePanel } from './live-state-panel'
import { RuntimeTimeline } from './runtime-timeline'
import { RuntimeControlBar } from './runtime-control-bar'
import { TaskArtifactsPanel } from '@/components/tasks/task-artifacts-panel'
import { isLiveTask } from '@/lib/runtime-activity'
import type { Task } from '@/schemas/task'
import type { ExecutionEvent, TaskTrace, Approval } from '@/schemas/execution-event'
import type { ArtifactMetadata } from '@/schemas/artifact'

interface TaskRuntimeViewProps {
  task: Task
  events?: ExecutionEvent[]
  trace?: TaskTrace
  approvals?: Approval[]
  artifacts?: ArtifactMetadata[]
  streamStatus?: string
  /** Owned by the parent that controls polling, so pause truly pauses. */
  following?: boolean
  onToggleFollow?: () => void
  /** False when execution-event storage is unavailable (fork/timeline 501). */
  forkSupported?: boolean
  /** Response-level latest seq (0 for an empty stream); pins fork afterSeq. */
  latestSeq?: number
}

/**
 * Consolidated runtime cockpit for ONE task, reusing the canvas panels. Parent
 * (task-detail) already fetches task/events/trace/approvals/artifacts, so this is
 * pure presentation — no duplicate fetches. Follow/pause is owned by the parent
 * so toggling it actually gates the parent's polling, not just the spotlight.
 */
export function TaskRuntimeView({ task, events = [], trace, approvals, artifacts, streamStatus, following = true, onToggleFollow, forkSupported = true, latestSeq }: TaskRuntimeViewProps) {
  const latestEvent = events[events.length - 1]
  // Prefer the response-level seq (0 for an empty stream) so fork afterSeq is
  // pinned even before any event arrives; fall back to the last event's seq.
  const pinnedSeq = latestSeq ?? latestEvent?.seq
  return (
    <div className="space-y-4">
      <RuntimeControlBar task={task} following={following} onToggleFollow={onToggleFollow} latestSeq={pinnedSeq} forkSupported={forkSupported} />
      <div className="grid gap-4 lg:grid-cols-3">
        <div className="space-y-4 lg:col-span-2">
          <ActivitySpotlight task={task} latestEvent={latestEvent} following={isLiveTask(task) && following} />
          <TaskFlowPanel task={task} events={events} />
          <RuntimeTimeline events={events} status={streamStatus} />
        </div>
        <div className="space-y-4">
          <ValidationSummary task={task} trace={trace} approvals={approvals} artifacts={artifacts} />
          <TaskArtifactsPanel taskId={task.metadata.name} taskUid={task.metadata.uid} />
          <LiveStatePanel task={task} />
        </div>
      </div>
    </div>
  )
}
