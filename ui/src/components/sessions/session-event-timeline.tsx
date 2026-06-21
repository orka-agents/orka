import { useEffect, useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Card, CardContent } from '@/components/ui/card'
import { EventTimeline } from '@/components/events/event-timeline'
import { useSessionEvents } from '@/hooks/use-execution-events'
import { useExecutionEventStream } from '@/hooks/use-execution-event-stream'
import { executionEventApi, mergeEventsBySeq, maxSeq } from '@/lib/execution-events'
import { ApiError } from '@/lib/api-client'
import type { ExecutionEvent } from '@/schemas/execution-event'

export interface SessionEventTimelineProps {
  sessionId: string
}

export function SessionEventTimeline({ sessionId }: SessionEventTimelineProps) {
  const initial = useSessionEvents(sessionId)
  // Sessions aggregate many tasks and have no single terminal event, so default
  // to following — the stream stays open until the session is deleted.
  const [following, setFollowing] = useState(true)

  const initialEvents = useMemo<ExecutionEvent[]>(
    () => initial.data?.events ?? [],
    [initial.data],
  )
  // Seed from the highest loaded session seq, not latestSeq: the session events
  // endpoint returns a bounded first page, so seeding from latestSeq would skip
  // every event between the loaded tail and latestSeq. Replaying from the loaded
  // tail fills the gap without dropping events.
  const seedSeq = maxSeq(initialEvents)
  const latestSeq = initial.data?.latestSeq ?? 0

  // Track how far the live stream has replayed so the backfill gap can close once
  // it catches up. Updated in an effect (not during render).
  const [streamedThrough, setStreamedThrough] = useState(0)

  // Backfill the tail when the server reports more events than we currently hold
  // (the freshly-loaded page or the streamed tail), even if the user paused
  // following, so a session with >1000 events isn't stuck on its first page. Once
  // the stream catches up the gap closes and "Stop following" actually pauses it.
  const highestHeld = Math.max(seedSeq, streamedThrough)
  const hasBackfillGap = latestSeq > highestHeld
  const streamEnabled = (following || hasBackfillGap) && !!sessionId

  const stream = useExecutionEventStream({
    url: executionEventApi.sessionStream(sessionId),
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
  // Show the true latest session sequence for the resume helper.
  const lastSeq = Math.max(latestSeq, highestHeld, stream.lastSeq, maxSeq(events))

  const notImplemented =
    initial.error instanceof ApiError && initial.error.status === 501
  const loadError = notImplemented
    ? 'Execution event storage is not enabled on this server.'
    : initial.error
      ? 'Failed to load session events.'
      : stream.status === 'error'
        ? stream.error
        : null

  // The session stream sends a SessionDeleted stream_complete frame when the
  // session is gone; surface that distinctly from a task's terminal completion.
  const sessionDeleted = stream.streamComplete?.type === 'SessionDeleted'

  return (
    <Card>
      <CardContent className="pt-6">
        {sessionDeleted && (
          <div className="mb-3 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive">
            This session was deleted. The timeline below shows its final recorded events.
          </div>
        )}
        <EventTimeline
          title="Session timeline"
          events={events}
          streamStatus={streamEnabled ? stream.status : 'idle'}
          lastSeq={lastSeq}
          isLoading={initial.isLoading}
          error={loadError}
          onRetry={() => initial.refetch()}
          following={following}
          onToggleFollow={() => setFollowing((v) => !v)}
          showTask
          taskLink={(taskName) => (
            <Link
              to="/tasks/$taskId"
              params={{ taskId: taskName }}
              className="font-mono text-xs text-primary hover:underline"
            >
              {taskName}
            </Link>
          )}
          emptyMessage="No execution events recorded for this session yet."
        />
      </CardContent>
    </Card>
  )
}
