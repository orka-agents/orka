import { useCallback, useEffect, useRef, useState } from 'react'
import { API_BASE_URL } from '@/lib/constants'
import { reconnectBackoffMs } from '@/hooks/use-execution-event-stream'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { isDecisionView, type DecisionView } from '@/schemas/oversight'

export type OversightStreamStatus =
  | 'idle'
  | 'connecting'
  | 'live'
  | 'reconnecting'
  /** 501: oversight is off. Terminal. */
  | 'unavailable'
  /** 401/403: the token cannot read the stream. Terminal. */
  | 'forbidden'
  /** 400: the query grammar refused the search. Terminal. */
  | 'rejected'

export interface OversightStreamOptions {
  q: string
  enabled: boolean
  /** Replay cursor at connect time: decisions after this seq are streamed. */
  after: number
  /** Most decisions kept; older ones drop off the end. */
  cap?: number
  reconnectDelayMs?: number
}

export interface OversightStreamResult {
  /** Newest first. */
  decisions: DecisionView[]
  status: OversightStreamStatus
  error: string | null
  lastSeq: number
}

export interface SSEFrame {
  id?: string
  event: string
  data: string
}

/** Splits SSE text into complete frames; the incomplete tail is returned. */
export function parseSSEFrames(buffer: string): { frames: SSEFrame[]; rest: string } {
  const normalized = buffer.replace(/\r\n/g, '\n')
  const blocks = normalized.split('\n\n')
  const rest = blocks.pop() ?? ''
  const frames: SSEFrame[] = []
  for (const block of blocks) {
    let event = 'message'
    let id: string | undefined
    const data: string[] = []
    let hasField = false
    for (const line of block.split('\n')) {
      if (line === '' || line.startsWith(':')) continue
      const colon = line.indexOf(':')
      const field = colon < 0 ? line : line.slice(0, colon)
      let value = colon < 0 ? '' : line.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)
      hasField = true
      if (field === 'event') event = value
      else if (field === 'id') id = value
      else if (field === 'data') data.push(value)
    }
    if (hasField) frames.push({ id, event, data: data.join('\n') })
  }
  return { frames, rest }
}

type Disposition = 'closed' | 'error' | 'aborted' | 'terminal'

const DEFAULT_RECONNECT_DELAY = 2000
const MAX_ERROR_STEPS = 5

/**
 * Live oversight decisions over SSE. EventSource cannot send a bearer token,
 * so the body is read with fetch. Decisions are deduplicated by seq, the
 * cursor survives reconnects, and errors back off exponentially up to a cap.
 */
export function useOversightStream({ q, enabled, after, cap = 500, reconnectDelayMs = DEFAULT_RECONNECT_DELAY }: OversightStreamOptions): OversightStreamResult {
  const namespace = useUIStore((s) => s.namespace)
  const [decisions, setDecisions] = useState<DecisionView[]>([])
  const [status, setStatus] = useState<OversightStreamStatus>('idle')
  const [error, setError] = useState<string | null>(null)
  const [lastSeq, setLastSeq] = useState(after)

  const afterRef = useRef(after)
  useEffect(() => {
    afterRef.current = after
  }, [after])
  const cursorRef = useRef(after)
  const seenRef = useRef<Set<number>>(new Set())
  const generationRef = useRef(0)
  const abortRef = useRef<AbortController | null>(null)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const accept = useCallback((decision: DecisionView, frameId?: string) => {
    const seq = decision.seq ?? (frameId ? Number(frameId) : NaN)
    if (Number.isFinite(seq)) {
      if (seenRef.current.has(seq)) return
      seenRef.current.add(seq)
      if (seq > cursorRef.current) {
        cursorRef.current = seq
        setLastSeq(seq)
      }
    }
    setDecisions((prev) => [decision, ...prev.filter((d) => d.id !== decision.id)].slice(0, cap))
  }, [cap])

  const connect = useCallback(async (generation: number): Promise<Disposition> => {
    const controller = new AbortController()
    abortRef.current = controller
    const token = useAuthStore.getState().token
    const params = new URLSearchParams()
    if (namespace) params.set('namespace', namespace)
    params.set('after', String(cursorRef.current))
    if (q) params.set('q', q)
    let response: Response
    try {
      response = await fetch(`${API_BASE_URL}/oversight/decisions/stream?${params.toString()}`, {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
        signal: controller.signal,
      })
    } catch (err) {
      if (controller.signal.aborted) return 'aborted'
      if (generation === generationRef.current) setError(err instanceof Error ? err.message : 'stream connection failed')
      return 'error'
    }
    const current = generation === generationRef.current
    if (!response.ok) {
      if (response.status === 401 && token && useAuthStore.getState().token === token) useAuthStore.getState().clearToken()
      if (response.status === 401 || response.status === 403) {
        if (current) { setStatus('forbidden'); setError('Not authorized to read oversight decisions') }
        return 'terminal'
      }
      if (response.status === 501) {
        if (current) { setStatus('unavailable'); setError('Oversight is not enabled on this controller') }
        return 'terminal'
      }
      if (response.status === 400) {
        const text = await response.text().catch(() => '')
        if (current) { setStatus('rejected'); setError(text || 'The search was refused') }
        return 'terminal'
      }
      if (current) setError(`stream failed: ${response.status}`)
      return 'error'
    }
    const reader = response.body?.getReader()
    if (!reader) return 'closed'
    if (current) { setStatus('live'); setError(null) }
    const decoder = new TextDecoder()
    let buffer = ''
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        const { frames, rest } = parseSSEFrames(buffer)
        buffer = rest
        for (const frame of frames) {
          if (generation !== generationRef.current) return 'aborted'
          if (frame.event !== 'decision') continue
          try {
            const parsed: unknown = JSON.parse(frame.data)
            if (isDecisionView(parsed)) accept(parsed, frame.id)
          } catch {
            // A malformed frame is skipped; the cursor does not move past it.
          }
        }
      }
    } catch (err) {
      if (controller.signal.aborted) return 'aborted'
      if (generation === generationRef.current) setError(err instanceof Error ? err.message : 'stream read failed')
      return 'error'
    }
    return 'closed'
  }, [namespace, q, accept])

  useEffect(() => {
    generationRef.current += 1
    const generation = generationRef.current
    abortRef.current?.abort()
    if (timerRef.current) clearTimeout(timerRef.current)
    if (!enabled) {
      setStatus('idle')
      return
    }
    seenRef.current = new Set()
    cursorRef.current = afterRef.current
    setDecisions([])
    setError(null)
    setLastSeq(afterRef.current)
    let errors = 0
    const run = async () => {
      while (generation === generationRef.current) {
        setStatus((prev) => (prev === 'live' || prev === 'reconnecting' ? 'reconnecting' : 'connecting'))
        const disposition = await connect(generation)
        if (generation !== generationRef.current || disposition === 'aborted' || disposition === 'terminal') return
        errors = disposition === 'error' ? Math.min(errors + 1, MAX_ERROR_STEPS) : 0
        setStatus('reconnecting')
        await new Promise<void>((resolve) => {
          timerRef.current = setTimeout(resolve, reconnectBackoffMs(reconnectDelayMs, errors))
        })
      }
    }
    void run()
    return () => {
      generationRef.current += 1
      abortRef.current?.abort()
      if (timerRef.current) clearTimeout(timerRef.current)
    }
  }, [enabled, connect, reconnectDelayMs])

  return { decisions, status, error, lastSeq }
}
