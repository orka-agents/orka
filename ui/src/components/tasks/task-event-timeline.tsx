import { useEffect, useMemo, useState } from 'react'
import { Card, CardContent } from '@/components/ui/card'
import { EventTimeline } from '@/components/events/event-timeline'
import { ForkDialog } from './fork-dialog'
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
}

export function TaskEventTimeline({ taskId, taskPhase }: TaskEventTimelineProps) {
  const initial = useTaskEvents(taskId)
  // Explicit user choice for following, or null for "auto". When auto, we follow
  // whenever the task is in a running phase — so a task that polls into Running
  // after mount (e.g. just after creation, before status.phase was populated)
  // starts following without the user clicking, while an explicit pause/start is
  // still respected. Deriving this (rather than syncing via an effect) keeps it
  // lint-clean and avoids a stale initial value.
  const [followOverride, setFollowOverride] = useState<boolean | null>(null)
  const following = followOverride ?? isRunning(taskPhase)
  // Fork-from-checkpoint dialog state, launched from an event row.
  const [forkEvent, setForkEvent] = useState<ExecutionEvent | null>(null)

  const toggleFollow = () => setFollowOverride(!following)

  const initialEvents = useMemo<ExecutionEvent[]>(
    () => initial.data?.events ?? [],
    [initial.data],
  )
  // Seed the stream cursor with the highest seq actually loaded, NOT latestSeq:
  // the list endpoint returns a bounded first page, so latestSeq can be far ahead
  // of what we hold. Seeding from the loaded tail lets the stream replay every
  // event after it (filling the gap up to latestSeq and beyond) without skipping.
  const seedSeq = maxSeq(initialEvents)
  const latestSeq = initial.data?.latestSeq ?? 0

  // Track how far the live stream has replayed, so the backfill gap can close once
  // it catches up. Updated in an effect (not during render) to satisfy the
  // refs-during-render rule. The component is keyed by taskId at its mount site,
  // so this resets naturally when the task target changes.
  const [streamedThrough, setStreamedThrough] = useState(0)

  // The list endpoint caps at 1000 events. When the server reports a higher
  // latestSeq than the highest seq we currently hold (the freshly-loaded page or
  // the streamed tail), there's more to backfill — open the stream even if the
  // user isn't actively following, so a completed task or long session with
  // >1000 events isn't stuck on its first page. Once the stream catches up the
  // gap closes and "Stop following" actually pauses the stream.
  const highestHeld = Math.max(seedSeq, streamedThrough)
  const hasBackfillGap = latestSeq > highestHeld
  const streamEnabled = (following || hasBackfillGap) && !!taskId

  const stream = useExecutionEventStream({
    url: executionEventApi.taskStream(taskId),
    enabled: streamEnabled,
    after: seedSeq,
  })

  useEffect(() => {
    // Grow-only: converges and bails out once caught up (Math.max returns the
    // same value), so this does not cascade renders.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setStreamedThrough((prev) => Math.max(prev, stream.lastSeq))
  }, [stream.lastSeq])

  const events = useMemo(
    () => mergeEventsBySeq(initialEvents, stream.events),
    [initialEvents, stream.events],
  )
  // Display the true latest sequence the server reported, even if our loaded
  // page or live tail hasn't reached it yet, so the resume-from-seq helper is
  // accurate.
  const lastSeq = Math.max(latestSeq, highestHeld, stream.lastSeq, maxSeq(events))

  const notImplemented =
    initial.error instanceof ApiError && initial.error.status === 501
  const loadError = notImplemented
    ? 'Execution event storage is not enabled on this server.'
    : initial.error
      ? 'Failed to load events.'
      : stream.status === 'error'
        ? stream.error
        : null

  // Forking depends on the same execution-event storage; hide the action when it
  // is unavailable so the UI never offers an endpoint the backend can't serve.
  const forkAvailable = !notImplemented

  return (
    <Card>
      <CardContent className="pt-6">
        <EventTimeline
          title="Execution timeline"
          events={events}
          streamStatus={streamEnabled ? stream.status : 'idle'}
          lastSeq={lastSeq}
          isLoading={initial.isLoading}
          error={loadError}
          onRetry={() => initial.refetch()}
          following={following}
          onToggleFollow={toggleFollow}
          onFork={forkAvailable ? (event) => setForkEvent(event) : undefined}
          emptyMessage="No execution events recorded for this task yet."
        />
        <ForkDialog
          taskId={taskId}
          event={forkEvent}
          open={forkEvent !== null}
          onOpenChange={(open) => {
            if (!open) setForkEvent(null)
          }}
        />
      </CardContent>
    </Card>
  )
}
