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
import { isTaskInFlight, isTaskTerminal } from '@/lib/task-phase'

const TERMINAL_EVENT_TYPES = new Set(['TaskSucceeded', 'TaskFailed', 'TaskCancelled'])

const TERMINAL_REFETCH_MAX_ATTEMPTS = 5
const TERMINAL_REFETCH_INTERVAL_MS = 800

export interface TaskEventTimelineProps {
  taskId: string
  taskPhase?: TaskPhase
  taskUid?: string
}

export function TaskEventTimeline({ taskId, taskPhase, taskUid }: TaskEventTimelineProps) {
  const initial = useTaskEvents(taskId, true, taskUid)
  const [followOverride, setFollowOverride] = useState<boolean | null>(null)
  const following = followOverride ?? isTaskInFlight(taskPhase)
  const [forkEvent, setForkEvent] = useState<ExecutionEvent | null>(null)

  const toggleFollow = () => setFollowOverride(!following)

  const initialEvents = useMemo<ExecutionEvent[]>(
    () => initial.data?.events ?? [],
    [initial.data],
  )
  const seedSeq = maxSeq(initialEvents)
  const latestSeq = initial.data?.latestSeq ?? 0

  const [streamedThrough, setStreamedThrough] = useState(0)
  const [terminalFrameSeen, setTerminalFrameSeen] = useState(false)
  const [wasFollowing, setWasFollowing] = useState(false)
  const [terminalRefetchAttempts, setTerminalRefetchAttempts] = useState(0)

  const highestHeld = Math.max(seedSeq, streamedThrough)
  const hasBackfillGap = latestSeq > highestHeld
  const hasLoadedTerminalEvent = initialEvents.some((e) => TERMINAL_EVENT_TYPES.has(e.type))
  const awaitingTerminalFrame =
    wasFollowing &&
    initial.isSuccess &&
    isTaskTerminal(taskPhase) &&
    followOverride !== false &&
    !terminalFrameSeen &&
    !hasLoadedTerminalEvent
  const streamEnabled = (following || hasBackfillGap || awaitingTerminalFrame) && !!taskId

  const stream = useExecutionEventStream({
    url: executionEventApi.taskStream(taskId),
    enabled: streamEnabled,
    after: seedSeq,
  })

  useEffect(() => {
    if (following) {
      setWasFollowing(true)
    }
  }, [following])

  useEffect(() => {
    if (stream.status === 'complete') {
      setTerminalFrameSeen(true)
    }
  }, [stream.status])

  const needsTerminalCatchUp =
    isTaskTerminal(taskPhase) &&
    initial.isSuccess &&
    !hasLoadedTerminalEvent &&
    terminalRefetchAttempts < TERMINAL_REFETCH_MAX_ATTEMPTS
  useEffect(() => {
    if (!needsTerminalCatchUp) return
    const timer = setTimeout(
      () => {
        setTerminalRefetchAttempts((n) => n + 1)
        void initial.refetch()
      },
      terminalRefetchAttempts === 0 ? 0 : TERMINAL_REFETCH_INTERVAL_MS,
    )
    return () => clearTimeout(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [needsTerminalCatchUp, terminalRefetchAttempts])

  useEffect(() => {
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
      ? 'Failed to load events.'
      : stream.status === 'error'
        ? stream.error
        : null

  const forkAvailable = !notImplemented

  return (
    <Card>
      <CardContent>
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
