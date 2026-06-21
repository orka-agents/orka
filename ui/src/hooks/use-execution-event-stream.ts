import { useCallback, useEffect, useRef, useState } from 'react'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { SSEFrameBuffer } from '@/lib/execution-events'
import type { ExecutionEvent, StreamComplete } from '@/schemas/execution-event'

export type ExecutionStreamStatus =
  | 'idle'
  | 'connecting'
  | 'streaming'
  | 'complete'
  | 'error'
  | 'unsupported'

export interface UseExecutionEventStreamOptions {
  // Full stream URL (e.g. executionEventApi.taskStream(id)). Namespace and after
  // cursor are appended by the hook.
  url: string
  enabled: boolean
  // Initial replay cursor. The stream replays events after this seq, so seed it
  // with the highest seq already loaded from the static events query to avoid
  // re-fetching history. Defaults to 0 (replay everything).
  after?: number
  // Delay before reconnecting after an unexpected close, in ms.
  reconnectDelayMs?: number
}

export interface UseExecutionEventStreamResult {
  events: ExecutionEvent[]
  lastSeq: number
  status: ExecutionStreamStatus
  error: string | null
  streamComplete: StreamComplete | null
  isFollowing: boolean
  stop: () => void
  restart: () => void
}

type ConnectDisposition = 'complete' | 'closed' | 'error' | 'aborted' | 'unsupported'

const DEFAULT_RECONNECT_DELAY = 2000
// Cap exponential backoff so a persistently failing endpoint settles at a slow
// retry cadence instead of being re-hit every reconnectDelayMs.
const MAX_RECONNECT_DELAY = 30000
const MAX_ERROR_BACKOFF_STEPS = 5

// Reconnect delay after `errorCount` consecutive errors: base * 2^(n-1), capped.
// errorCount 0 (a clean 'closed' cycle) returns the tight base delay.
export function reconnectBackoffMs(
  baseDelay: number,
  errorCount: number,
  maxDelay = MAX_RECONNECT_DELAY,
  maxSteps = MAX_ERROR_BACKOFF_STEPS,
): number {
  if (errorCount <= 0) return baseDelay
  const step = Math.min(errorCount, maxSteps)
  return Math.min(baseDelay * 2 ** (step - 1), maxDelay)
}

// Live execution-event stream over Server-Sent Events. EventSource cannot send an
// Authorization header, so this reads the SSE body via fetch + ReadableStream,
// mirroring the existing useTaskLogs streaming approach.
export function useExecutionEventStream(
  options: UseExecutionEventStreamOptions,
): UseExecutionEventStreamResult {
  const { url, enabled, after = 0, reconnectDelayMs = DEFAULT_RECONNECT_DELAY } = options
  const namespace = useUIStore((s) => s.namespace)

  const [events, setEvents] = useState<ExecutionEvent[]>([])
  const [lastSeq, setLastSeq] = useState(after)
  const [status, setStatus] = useState<ExecutionStreamStatus>('idle')
  const [error, setError] = useState<string | null>(null)
  const [streamComplete, setStreamComplete] = useState<StreamComplete | null>(null)

  // Dedupe set keyed by seq; survives reconnects so replays never duplicate.
  const seenSeqRef = useRef<Set<number>>(new Set())
  const lastSeqRef = useRef(after)
  const abortRef = useRef<AbortController | null>(null)
  const stoppedRef = useRef(false)
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  // Generation guards against stale async loops writing state after a restart.
  const generationRef = useRef(0)
  // Consecutive error count drives capped exponential backoff so a persistently
  // failing endpoint isn't re-hit at the tight interval forever.
  const errorBackoffRef = useRef(0)

  const clearReconnectTimer = useCallback(() => {
    if (reconnectTimerRef.current) {
      clearTimeout(reconnectTimerRef.current)
      reconnectTimerRef.current = null
    }
  }, [])

  const acceptEvent = useCallback((event: ExecutionEvent) => {
    if (seenSeqRef.current.has(event.seq)) return
    seenSeqRef.current.add(event.seq)
    setEvents((prev) => {
      const next = [...prev, event]
      next.sort((a, b) => a.seq - b.seq)
      return next
    })
    // Advance the cursor only after the event is accepted.
    if (event.seq > lastSeqRef.current) {
      lastSeqRef.current = event.seq
      setLastSeq(event.seq)
    }
  }, [])

  const connect = useCallback(
    async (generation: number): Promise<ConnectDisposition> => {
      const token = useAuthStore.getState().token
      const controller = new AbortController()
      abortRef.current = controller

      const params = new URLSearchParams()
      if (namespace) params.set('namespace', namespace)
      params.set('after', String(lastSeqRef.current))
      const fullUrl = `${url}?${params.toString()}`

      let response: Response
      try {
        response = await fetch(fullUrl, {
          headers: token ? { Authorization: `Bearer ${token}` } : {},
          signal: controller.signal,
        })
      } catch (err) {
        if (controller.signal.aborted) return 'aborted'
        if (generation === generationRef.current) {
          setError(err instanceof Error ? err.message : 'stream connection failed')
        }
        return 'error'
      }

      if (!response.ok) {
        // 401 => expired/invalid bearer token. Match the shared API client:
        // clear the token so the app routes the user back through auth, and stop
        // retrying instead of reconnecting forever with the same bad header.
        if (response.status === 401) {
          useAuthStore.getState().clearToken()
          if (generation === generationRef.current) {
            setError('unauthorized: please sign in again')
          }
          return 'unsupported'
        }
        // 501 => execution event storage not enabled. Treat as unsupported and do
        // not reconnect; the caller surfaces a clear unavailable message.
        if (response.status === 501) {
          if (generation === generationRef.current) {
            setError('execution event streaming is not enabled')
          }
          return 'unsupported'
        }
        if (generation === generationRef.current) {
          setError(`stream failed: ${response.status} ${response.statusText}`)
        }
        return 'error'
      }

      const reader = response.body?.getReader()
      if (!reader) {
        return 'closed'
      }

      if (generation === generationRef.current) {
        setStatus('streaming')
        setError(null)
      }

      const decoder = new TextDecoder()
      const frameBuffer = new SSEFrameBuffer()
      try {
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          const chunk = decoder.decode(value, { stream: true })
          const frames = frameBuffer.push(chunk)
          for (const frame of frames) {
            if (generation !== generationRef.current) return 'aborted'
            if (frame.kind === 'event') {
              acceptEvent(frame.event)
            } else if (frame.kind === 'complete') {
              if (frame.complete.lastSeq > lastSeqRef.current) {
                lastSeqRef.current = frame.complete.lastSeq
                setLastSeq(frame.complete.lastSeq)
              }
              setStreamComplete(frame.complete)
              return 'complete'
            }
            // heartbeat / unknown: ignore, do not advance cursor.
          }
        }
        // Drain any trailing partial block on clean close.
        for (const frame of frameBuffer.flush()) {
          if (frame.kind === 'event') acceptEvent(frame.event)
          else if (frame.kind === 'complete') {
            setStreamComplete(frame.complete)
            return 'complete'
          }
        }
      } catch (err) {
        if (controller.signal.aborted) return 'aborted'
        if (generation === generationRef.current) {
          setError(err instanceof Error ? err.message : 'stream read failed')
        }
        return 'error'
      }
      return 'closed'
    },
    [url, namespace, acceptEvent],
  )

  // Orchestrates connect + reconnect for one generation until terminal,
  // unsupported, manual stop, or unmount.
  const runLoop = useCallback(
    async (generation: number) => {
      while (!stoppedRef.current && generation === generationRef.current) {
        if (generation === generationRef.current) setStatus('connecting')
        const disposition = await connect(generation)
        if (generation !== generationRef.current || stoppedRef.current) return
        if (disposition === 'complete') {
          setStatus('complete')
          return
        }
        if (disposition === 'unsupported') {
          setStatus('unsupported')
          return
        }
        if (disposition === 'aborted') {
          return
        }
        // 'closed' is the normal end of a long-poll cycle: reconnect promptly and
        // clear any error backoff. 'error' escalates with capped exponential
        // backoff so a persistently failing endpoint isn't hammered every 2s.
        let delay = reconnectDelayMs
        if (disposition === 'error') {
          setStatus('error')
          errorBackoffRef.current = Math.min(errorBackoffRef.current + 1, MAX_ERROR_BACKOFF_STEPS)
          delay = reconnectBackoffMs(reconnectDelayMs, errorBackoffRef.current)
        } else {
          errorBackoffRef.current = 0
        }
        await new Promise<void>((resolve) => {
          reconnectTimerRef.current = setTimeout(resolve, delay)
        })
      }
    },
    [connect, reconnectDelayMs],
  )

  const teardown = useCallback(() => {
    clearReconnectTimer()
    abortRef.current?.abort()
    abortRef.current = null
  }, [clearReconnectTimer])

  const stop = useCallback(() => {
    stoppedRef.current = true
    generationRef.current += 1
    teardown()
    setStatus((prev) => (prev === 'complete' || prev === 'unsupported' ? prev : 'idle'))
  }, [teardown])

  // Reset all accumulated stream state and start a fresh connection. Used both
  // by the public restart() and by the mount/target-change effect, so switching
  // tasks/sessions or namespaces never carries the previous stream's events,
  // dedupe set, cursor, or terminal marker into the new connection.
  const startFresh = useCallback(() => {
    stoppedRef.current = false
    generationRef.current += 1
    const generation = generationRef.current
    teardown()
    seenSeqRef.current = new Set()
    lastSeqRef.current = after
    errorBackoffRef.current = 0
    setEvents([])
    setLastSeq(after)
    setError(null)
    setStreamComplete(null)
    void runLoop(generation)
  }, [after, runLoop, teardown])

  const restart = startFresh

  useEffect(() => {
    if (!enabled) {
      stoppedRef.current = true
      generationRef.current += 1
      teardown()
      setStatus('idle')
      return
    }
    // A change to url/namespace/after means a new stream target; reset state so
    // stale events and an outdated cursor don't bleed across the switch.
    startFresh()
    return () => {
      generationRef.current += 1
      teardown()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, url, namespace, after])

  const isFollowing = status === 'connecting' || status === 'streaming'

  return { events, lastSeq, status, error, streamComplete, isFollowing, stop, restart }
}
