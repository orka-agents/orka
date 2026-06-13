import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@/test/test-utils'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { makeEvent } from '@/test/fixtures/events'
import type { UseExecutionEventStreamResult } from '@/hooks/use-execution-event-stream'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

// Control the stream hook output so the container test is deterministic.
const streamState: { current: Partial<UseExecutionEventStreamResult> } = { current: {} }
vi.mock('@/hooks/use-execution-event-stream', () => ({
  useExecutionEventStream: () => ({
    events: [],
    lastSeq: 0,
    status: 'idle',
    error: null,
    streamComplete: null,
    isFollowing: false,
    stop: vi.fn(),
    restart: vi.fn(),
    ...streamState.current,
  }),
}))

import { TaskEventTimeline } from './task-event-timeline'

const API = '/api/v1'

describe('TaskEventTimeline', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    streamState.current = {}
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
})
