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
})
