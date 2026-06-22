import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen, waitFor, within } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { makeEvent } from '@/test/fixtures/events'
import type { UseExecutionEventStreamResult } from '@/hooks/use-execution-event-stream'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, ...props }: any) => <a {...props}>{children}</a> }
})

// Control the stream hook output so the container test is deterministic, and
// capture the options it was called with so we can assert the seed cursor.
const streamState: { current: Partial<UseExecutionEventStreamResult> } = { current: {} }
const streamCalls: { url: string; after?: number; enabled: boolean }[] = []
vi.mock('@/hooks/use-execution-event-stream', () => ({
  useExecutionEventStream: (opts: { url: string; after?: number; enabled: boolean }) => {
    streamCalls.push({ url: opts.url, after: opts.after, enabled: opts.enabled })
    return {
      events: [],
      lastSeq: 0,
      status: 'idle',
      error: null,
      streamComplete: null,
      isFollowing: false,
      stop: vi.fn(),
      restart: vi.fn(),
      ...streamState.current,
    }
  },
}))

import { TaskEventTimeline } from './task-event-timeline'

const API = '/api/v1'

describe('TaskEventTimeline', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    streamState.current = {}
    streamCalls.length = 0
  })

  it('loads and renders initial events from the API', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default',
          streamType: 'task',
          streamID: 'tk',
          afterSeq: 0,
          latestSeq: 2,
          events: [
            makeEvent({ seq: 1, type: 'TaskCreated', summary: 'created' }),
            makeEvent({ seq: 2, type: 'TaskStarted', summary: 'started' }),
          ],
        }),
      ),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByText('TaskCreated')).toBeInTheDocument())
    expect(screen.getAllByTestId('event-row')).toHaveLength(2)
  })

  it('dedupes events shared between the initial load and the live stream', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default',
          streamType: 'task',
          streamID: 'tk',
          afterSeq: 0,
          latestSeq: 2,
          events: [
            makeEvent({ seq: 1, type: 'TaskCreated' }),
            makeEvent({ seq: 2, type: 'TaskStarted' }),
          ],
        }),
      ),
    )
    // Stream replays seq 2 (overlap) and adds seq 3.
    streamState.current = {
      events: [makeEvent({ seq: 2, type: 'TaskStarted' }), makeEvent({ seq: 3, type: 'ToolCallStarted' })],
      lastSeq: 3,
      status: 'streaming',
    }
    render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    // TaskCreated (seq 1) comes only from the async initial load, so waiting on it
    // guarantees both the initial page and the synchronous stream events merged.
    await waitFor(() => expect(screen.getByText('TaskCreated')).toBeInTheDocument())
    expect(screen.getByText('ToolCallStarted')).toBeInTheDocument()
    // seq 1,2,3 — no duplicate seq 2.
    expect(screen.getAllByTestId('event-row')).toHaveLength(3)
  })

  it('reflects terminal stream completion', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 1,
          events: [makeEvent({ seq: 1, type: 'TaskStarted' })],
        }),
      ),
    )
    streamState.current = { events: [], lastSeq: 1, status: 'complete' }
    render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(screen.getByText('stream complete')).toBeInTheDocument())
  })

  it('surfaces a clear message when execution events are not enabled (501)', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () => new HttpResponse('not enabled', { status: 501 })),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() =>
      expect(screen.getByText(/execution event storage is not enabled/i)).toBeInTheDocument(),
    )
  })

  it('opens the fork dialog from an event row', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 3,
          events: [makeEvent({ seq: 3, type: 'ToolCallCompleted', summary: 'done' })],
        }),
      ),
    )
    const user = userEvent.setup()
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByText('done')).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /fork from here/i }))
    // The dialog opens, pre-seeded with the row's seq.
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/fork from checkpoint/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/#3/)).toBeInTheDocument()
  })

  it('does not offer fork when execution events are unsupported (501)', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () => new HttpResponse('not enabled', { status: 501 })),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() =>
      expect(screen.getByText(/execution event storage is not enabled/i)).toBeInTheDocument(),
    )
    expect(screen.queryByRole('button', { name: /fork from here/i })).not.toBeInTheDocument()
  })

  it('seeds the live stream from the highest loaded seq, not latestSeq, on a partial page', async () => {
    // Loaded page tops out at seq 100 while the server reports latestSeq 500.
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 500,
          events: [makeEvent({ seq: 99, type: 'TaskStarted' }), makeEvent({ seq: 100, type: 'ModelMessage' })],
        }),
      ),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    // After the page loads (events up to seq 100), the stream must be seeded with
    // the loaded tail (100) so it replays 101..500, rather than latestSeq (500)
    // which would skip every event in the gap. Before the query resolves the seed
    // is 0 (empty page), so wait for the post-load seed.
    await waitFor(() => expect(streamCalls.some((c) => c.enabled && c.after === 100)).toBe(true))
    expect(streamCalls.some((c) => c.enabled && c.after === 500)).toBe(false)
  })

  it('backfills a completed task with more events than the loaded page, even when not following', async () => {
    // Completed task (follow defaults off), loaded page tops at 100 but the server
    // reports latestSeq 1500 — there's a tail to backfill.
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 1500,
          events: [makeEvent({ seq: 100, type: 'ModelMessage', summary: 'loaded tail' })],
        }),
      ),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByText('loaded tail')).toBeInTheDocument())
    // The stream is enabled (to backfill 101..1500) even though following is off,
    // seeded from the loaded tail.
    await waitFor(() => expect(streamCalls.some((c) => c.enabled && c.after === 100)).toBe(true))
  })

  it('does not open the stream for a completed task with no backfill gap', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 2,
          events: [makeEvent({ seq: 1, type: 'TaskStarted' }), makeEvent({ seq: 2, type: 'TaskSucceeded', summary: 'all loaded' })],
        }),
      ),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByText('all loaded')).toBeInTheDocument())
    // Everything is loaded (latestSeq == maxSeq) and we're not following, so the
    // stream stays closed.
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
  })

  it('closes the backfill stream once it has caught up so pausing follow works', async () => {
    // First page caps at seq 100, server reports latestSeq 1500 (a gap), and the
    // mocked stream has already replayed through 1500 and completed (the terminal
    // stream_complete frame arrived).
    streamState.current = {
      events: [makeEvent({ seq: 1500, type: 'ModelMessage', summary: 'caught up' })],
      lastSeq: 1500,
      status: 'complete',
      streamComplete: { lastSeq: 1500, type: 'TaskSucceeded' },
    }
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 1500,
          events: [makeEvent({ seq: 100, type: 'ModelMessage', summary: 'first page tail' })],
        }),
      ),
    )
    // Completed task, follow off. The gap forced the stream open, but the merged
    // events now reach latestSeq (1500), so the gap is closed.
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByText('caught up')).toBeInTheDocument())
    // The most recent stream call reflects the closed gap: not enabled, because
    // following is off and there's no longer a tail to backfill.
    await waitFor(() => {
      const last = streamCalls[streamCalls.length - 1]
      expect(last.enabled).toBe(false)
    })
  })

  it('auto-starts following when a task mounted before its phase was known becomes running', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 1,
          events: [makeEvent({ seq: 1, type: 'TaskCreated' })],
        }),
      ),
    )
    // Mounted before status.phase is known (undefined) — following is off, no gap.
    const view = render(<TaskEventTimeline taskId="tk" taskPhase={undefined} />)
    await waitFor(() => expect(screen.getByText('TaskCreated')).toBeInTheDocument())
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
    // The task polls into a running phase; follow auto-starts so live events
    // aren't missed, even though the user never clicked Follow.
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(streamCalls.some((c) => c.enabled)).toBe(true))
  })

  it('keeps the stream open through a terminal phase until stream_complete arrives', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 5,
          events: [makeEvent({ seq: 5, type: 'ModelMessage', summary: 'mid run' })],
        }),
      ),
    )
    // Auto-followed running task; stream is open and has caught up to latestSeq
    // (so the backfill gap is closed and only `following`/terminal-catch-up keep
    // it open).
    streamState.current = { events: [], lastSeq: 5, status: 'streaming' }
    const view = render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(screen.getByText('mid run')).toBeInTheDocument())
    expect(streamCalls[streamCalls.length - 1].enabled).toBe(true)

    // The controller persists the terminal phase before the terminal event, so
    // the poll flips taskPhase to Succeeded while the stream is still streaming
    // (no stream_complete yet). The stream must stay open to receive the final
    // TaskSucceeded + stream_complete.
    streamCalls.length = 0
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(streamCalls.length).toBeGreaterThan(0))
    expect(streamCalls[streamCalls.length - 1].enabled).toBe(true)

    // Once stream_complete is observed, the catch-up releases and the stream
    // disables.
    streamCalls.length = 0
    streamState.current = {
      events: [],
      lastSeq: 6,
      status: 'complete',
      streamComplete: { lastSeq: 6, type: 'TaskSucceeded' },
    }
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => {
      const last = streamCalls[streamCalls.length - 1]
      expect(last && last.enabled).toBe(false)
    })
  })

  it('does not force the stream open on a terminal phase the user explicitly paused', async () => {
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 3,
          events: [makeEvent({ seq: 3, type: 'ModelMessage', summary: 'paused run' })],
        }),
      ),
    )
    streamState.current = { events: [], lastSeq: 3, status: 'streaming' }
    const user = userEvent.setup()
    const view = render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(screen.getByText('paused run')).toBeInTheDocument())
    // User explicitly stops following.
    await user.click(screen.getByRole('button', { name: /stop following/i }))
    streamCalls.length = 0
    // Task completes — but since the user paused, we do NOT force the stream open.
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(streamCalls.length).toBeGreaterThan(0))
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
  })

  it('does not open a stream for an already-terminal task opened fresh with no events', async () => {
    // A settled task with an empty event stream (e.g. created before execution
    // events were recorded). The backend never emits stream_complete here, so
    // terminal catch-up must not fire — we were never following this task.
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 0,
          events: [],
        }),
      ),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() =>
      expect(screen.getByText(/no execution events recorded for this task/i)).toBeInTheDocument(),
    )
    // Give any spurious catch-up a chance to enable the stream.
    await new Promise((r) => setTimeout(r, 50))
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
  })
})
