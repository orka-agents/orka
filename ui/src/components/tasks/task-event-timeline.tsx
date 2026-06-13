import { useMemo, useState } from 'react'
import { Card, CardContent } from '@/components/ui/card'
import { EventTimeline } from '@/components/events/event-timeline'
import { useTaskEvents } from '@/hooks/use-execution-events'
import { useExecutionEventStream } from '@/hooks/use-execution-event-stream'
import { executionEventApi, mergeEventsBySeq, maxSeq } from '@/lib/execution-events'
import { ApiError } from '@/lib/api-client'
import type { ExecutionEvent } from '@/schemas/execution-event'
import type { TaskPhase } from '@/schemas/task'

function isRunning(phase?: TaskPhase): boolean {
  return phase === 'Running' || phase === 'Pending'
}

export interface TaskEventTimelineProps {
  taskId: string
  taskPhase?: TaskPhase
  // Optional fork action, wired by the parent (task detail) in Phase 6.
  onFork?: (event: ExecutionEvent) => void
}

export function TaskEventTimeline({ taskId, taskPhase, onFork }: TaskEventTimelineProps) {
  const initial = useTaskEvents(taskId)
  // Default to following for in-flight tasks; user can toggle.
  const [following, setFollowing] = useState(() => isRunning(taskPhase))

  const initialEvents = useMemo<ExecutionEvent[]>(
    () => initial.data?.events ?? [],
    [initial.data],
  )
  // Seed the stream cursor with the highest seq already loaded so we replay only
  // the tail. The query reports latestSeq even when its page is empty.
  const seedSeq = Math.max(maxSeq(initialEvents), initial.data?.latestSeq ?? 0)

  const stream = useExecutionEventStream({
    url: executionEventApi.taskStream(taskId),
    enabled: following && !!taskId,
    after: seedSeq,
  })

  const events = useMemo(
    () => mergeEventsBySeq(initialEvents, stream.events),
    [initialEvents, stream.events],
  )
  const lastSeq = Math.max(seedSeq, stream.lastSeq, maxSeq(events))

  const notImplemented =
    initial.error instanceof ApiError && initial.error.status === 501
  const loadError = notImplemented
    ? 'Execution event storage is not enabled on this server.'
    : initial.error
      ? 'Failed to load events.'
      : stream.status === 'error'
        ? stream.error
        : null

  return (
    <Card>
      <CardContent className="pt-6">
        <EventTimeline
          title="Execution timeline"
          events={events}
          streamStatus={following ? stream.status : 'idle'}
          lastSeq={lastSeq}
          isLoading={initial.isLoading}
          error={loadError}
          onRetry={() => initial.refetch()}
          following={following}
          onToggleFollow={() => setFollowing((v) => !v)}
          onFork={onFork}
          emptyMessage="No execution events recorded for this task yet."
        />
      </CardContent>
    </Card>
  )
}
