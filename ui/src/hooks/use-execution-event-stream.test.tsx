import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { renderHook, waitFor, act } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useExecutionEventStream, reconnectBackoffMs } from './use-execution-event-stream'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

function eventFrame(seq: number, type = 'TaskStarted', extra: Record<string, unknown> = {}) {
  const data = JSON.stringify({
    id: `evt-${seq}`,
    namespace: 'default',
    streamType: 'task',
    streamID: 'tk',
    seq,
    type,
    severity: 'info',
    createdAt: '2026-06-13T00:00:00Z',
    ...extra,
  })
  return `id: ${seq}\nevent: execution_event\ndata: ${data}\n\n`
}

function completeFrame(lastSeq: number, type = 'TaskSucceeded') {
  return `id: ${lastSeq}\nevent: stream_complete\ndata: {"lastSeq":${lastSeq},"type":"${type}"}\n\n`
}

// Controllable SSE response: push chunks, then close.
function makeStreamResponse() {
  let controller: ReadableStreamDefaultController<Uint8Array>
  const encoder = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c
    },
  })
  const response = new Response(stream, { status: 200 })
  return {
    response,
    push: (text: string) => act(() => { controller.enqueue(encoder.encode(text)) }),
    close: () => act(() => { controller.close() }),
  }
}

describe('useExecutionEventStream', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('connects with namespace and after cursor and accumulates events', async () => {
    const conn = makeStreamResponse()
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(conn.response)

    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true, after: 0 }),
    )

    await waitFor(() => expect(fetchSpy).toHaveBeenCalled())
    const calledUrl = fetchSpy.mock.calls[0][0] as string
    expect(calledUrl).toContain('/api/v1/tasks/tk/stream')
    expect(calledUrl).toContain('namespace=default')
    expect(calledUrl).toContain('after=0')
    expect(fetchSpy.mock.calls[0][1]).toMatchObject({
      headers: { Authorization: 'Bearer test-token' },
    })

    conn.push(eventFrame(1))
    conn.push(eventFrame(2))
    await waitFor(() => expect(result.current.events).toHaveLength(2))
    expect(result.current.lastSeq).toBe(2)
    expect(result.current.status).toBe('streaming')
  })

  it('dedupes events by seq across replays', async () => {
    const conn = makeStreamResponse()
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(conn.response)

    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true }),
    )
    await waitFor(() => expect(result.current.status).toBe('streaming'))

    conn.push(eventFrame(1))
    conn.push(eventFrame(1)) // duplicate seq
    conn.push(eventFrame(2))
    await waitFor(() => expect(result.current.events).toHaveLength(2))
    expect(result.current.events.map((e) => e.seq)).toEqual([1, 2])
  })

  it('stops following on stream_complete and exposes terminal info', async () => {
    const conn = makeStreamResponse()
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(conn.response)

    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true }),
    )
    await waitFor(() => expect(result.current.status).toBe('streaming'))

    conn.push(eventFrame(1))
    conn.push(completeFrame(1, 'TaskSucceeded'))
    await waitFor(() => expect(result.current.status).toBe('complete'))
    expect(result.current.isFollowing).toBe(false)
    expect(result.current.streamComplete?.type).toBe('TaskSucceeded')
    expect(result.current.lastSeq).toBe(1)
  })

  it('ignores heartbeat frames and does not advance the cursor', async () => {
    const conn = makeStreamResponse()
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(conn.response)

    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true }),
    )
    await waitFor(() => expect(result.current.status).toBe('streaming'))

    conn.push(eventFrame(3))
    conn.push(': heartbeat\n\n')
    await waitFor(() => expect(result.current.events).toHaveLength(1))
    expect(result.current.lastSeq).toBe(3)
  })

  it('surfaces an error status when the response is not ok', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response('boom', { status: 500, statusText: 'Internal Server Error' }),
    )
    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true, reconnectDelayMs: 50 }),
    )
    await waitFor(() => expect(result.current.status).toBe('error'))
    expect(result.current.error).toContain('500')
  })

  it('marks unsupported when streaming is not enabled (501)', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response('execution event storage not enabled', { status: 501 }),
    )
    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true }),
    )
    await waitFor(() => expect(result.current.status).toBe('unsupported'))
  })

  it('clears the auth token and stops retrying on a 401', async () => {
    const clearToken = vi.fn()
    vi.spyOn(useAuthStore, 'getState').mockReturnValue({
      token: 'expired',
      setToken: vi.fn(),
      clearToken,
    })
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response('unauthorized', { status: 401 }),
    )
    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true, reconnectDelayMs: 20 }),
    )
    // 401 => clear token (like the API client) and go to a terminal unsupported
    // state instead of reconnecting forever with the dead token.
    await waitFor(() => expect(result.current.status).toBe('unsupported'))
    expect(clearToken).toHaveBeenCalled()
    const callsAfter = fetchSpy.mock.calls.length
    await new Promise((r) => setTimeout(r, 80))
    expect(fetchSpy.mock.calls.length).toBe(callsAfter) // no further reconnect attempts
  })

  it('reconnects from the latest seq after an unexpected close', async () => {
    const first = makeStreamResponse()
    const second = makeStreamResponse()
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(first.response)
      .mockResolvedValueOnce(second.response)

    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true, reconnectDelayMs: 10 }),
    )
    await waitFor(() => expect(result.current.status).toBe('streaming'))
    first.push(eventFrame(5))
    await waitFor(() => expect(result.current.lastSeq).toBe(5))
    first.close()

    await waitFor(() => expect(fetchSpy).toHaveBeenCalledTimes(2))
    const reconnectUrl = fetchSpy.mock.calls[1][0] as string
    expect(reconnectUrl).toContain('after=5')
  })

  it('stop halts following and aborts the request', async () => {
    const conn = makeStreamResponse()
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(conn.response)
    const { result } = renderHook(() =>
      useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: true }),
    )
    await waitFor(() => expect(result.current.status).toBe('streaming'))
    act(() => { result.current.stop() })
    await waitFor(() => expect(result.current.isFollowing).toBe(false))
  })

  it('does not connect when disabled', () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    renderHook(() => useExecutionEventStream({ url: '/api/v1/tasks/tk/stream', enabled: false }))
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('resets accumulated events and cursor when the stream target changes', async () => {
    const first = makeStreamResponse()
    const second = makeStreamResponse()
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(first.response)
      .mockResolvedValueOnce(second.response)

    // The hook stays mounted; only the url prop changes, mirroring navigating
    // from one task/session to another without unmounting.
    const { result, rerender } = renderHook(
      ({ url }) => useExecutionEventStream({ url, enabled: true }),
      { initialProps: { url: '/api/v1/tasks/task-a/stream' } },
    )
    await waitFor(() => expect(result.current.status).toBe('streaming'))
    first.push(eventFrame(7))
    await waitFor(() => expect(result.current.events).toHaveLength(1))
    expect(result.current.lastSeq).toBe(7)

    // Switch target without unmounting.
    rerender({ url: '/api/v1/tasks/task-b/stream' })
    await waitFor(() => expect(fetchSpy).toHaveBeenCalledTimes(2))
    // Stale events and the old cursor must be cleared, and the new connection
    // starts from after=0, not the previous stream's seq 7.
    expect(result.current.events).toHaveLength(0)
    expect(result.current.lastSeq).toBe(0)
    const secondUrl = fetchSpy.mock.calls[1][0] as string
    expect(secondUrl).toContain('/api/v1/tasks/task-b/stream')
    expect(secondUrl).toContain('after=0')
  })

  it('applies capped exponential backoff across consecutive errors', () => {
    // A clean 'closed' cycle (errorCount 0) keeps the tight base delay.
    expect(reconnectBackoffMs(2000, 0)).toBe(2000)
    // Consecutive errors escalate: base, 2x, 4x, 8x, ...
    expect(reconnectBackoffMs(2000, 1)).toBe(2000)
    expect(reconnectBackoffMs(2000, 2)).toBe(4000)
    expect(reconnectBackoffMs(2000, 3)).toBe(8000)
    // Each step is strictly larger until the cap.
    const seq = [1, 2, 3, 4, 5].map((n) => reconnectBackoffMs(2000, n))
    for (let i = 1; i < seq.length; i++) expect(seq[i]).toBeGreaterThan(seq[i - 1])
    // Capped at 30s and never exceeded, even past the max step count.
    expect(reconnectBackoffMs(2000, 99)).toBe(30000)
    expect(reconnectBackoffMs(20000, 5)).toBe(30000)
  })
})
