import { useEffect, useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
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
  const [following, setFollowing] = useState(true)

  const initialEvents = useMemo<ExecutionEvent[]>(
    () => initial.data?.events ?? [],
    [initial.data],
  )
  const seedSeq = maxSeq(initialEvents)
  const latestSeq = initial.data?.latestSeq ?? 0

  const [streamedThrough, setStreamedThrough] = useState(0)

  const highestHeld = Math.max(seedSeq, streamedThrough)
  const hasBackfillGap = latestSeq > highestHeld
  const streamEnabled = (following || hasBackfillGap) && !!sessionId

  const stream = useExecutionEventStream({
    url: executionEventApi.sessionStream(sessionId),
    enabled: streamEnabled,
    after: seedSeq,
  })

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setStreamedThrough((prev) => Math.max(prev, stream.lastSeq))
  }, [stream.lastSeq])

  const events = useMemo(
    () => mergeEventsBySeq(initialEvents, stream.events),
    [initialEvents, stream.events],
  )
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

  const sessionDeleted = stream.streamComplete?.type === 'SessionDeleted'
  const queryClient = useQueryClient()
  useEffect(() => {
    if (sessionDeleted) queryClient.invalidateQueries({ queryKey: ['session', sessionId] })
  }, [sessionDeleted, sessionId, queryClient])

  return (
    <Card>
      <CardContent>
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
