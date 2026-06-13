import { useMemo, useState } from 'react'
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
  const seedSeq = Math.max(maxSeq(initialEvents), initial.data?.latestSeq ?? 0)

  const stream = useExecutionEventStream({
    url: executionEventApi.sessionStream(sessionId),
    enabled: following && !!sessionId,
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
          streamStatus={following ? stream.status : 'idle'}
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
