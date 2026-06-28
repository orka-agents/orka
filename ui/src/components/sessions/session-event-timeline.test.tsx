import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen, waitFor, within } from '@/test/test-utils'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { makeEvent } from '@/test/fixtures/events'
import type { UseExecutionEventStreamResult } from '@/hooks/use-execution-event-stream'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

// Render Link as a plain anchor so we can assert hrefs without a router.
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => (
      <a href={typeof to === 'string' && params?.taskId ? to.replace('$taskId', params.taskId) : to} {...props}>
        {children}
      </a>
    ),
  }
})

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

import { SessionEventTimeline } from './session-event-timeline'

const API = '/api/v1'

// A session event carries the originating task identity.
function sessionEvent(seq: number, taskName: string, type: string, summary?: string) {
  return makeEvent({
    seq,
    type,
    summary,
    streamType: 'session',
    streamID: 'sess',
    taskName,
    taskStreamID: taskName,
    taskSeq: seq,
  })
}

describe('SessionEventTimeline', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    streamState.current = {}
  })

  it('renders events from multiple tasks with task identity', async () => {
    server.use(
      http.get(`${API}/sessions/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default',
          streamType: 'session',
          streamID: 'sess',
          afterSeq: 0,
          latestSeq: 3,
          events: [
            sessionEvent(1, 'task-a', 'TaskStarted', 'a started'),
            sessionEvent(2, 'task-b', 'ToolCallStarted', 'b tool'),
            sessionEvent(3, 'task-a', 'TaskSucceeded', 'a done'),
          ],
        }),
      ),
    )
    render(<SessionEventTimeline sessionId="sess" />)
    await waitFor(() => expect(screen.getByText('a started')).toBeInTheDocument())
    expect(screen.getAllByTestId('event-row')).toHaveLength(3)
    // Both task identities are shown.
    expect(screen.getAllByText('task-a').length).toBeGreaterThanOrEqual(1)
    expect(screen.getByText('task-b')).toBeInTheDocument()
  })

  it('renders task links that navigate to task detail', async () => {
    server.use(
      http.get(`${API}/sessions/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'session', streamID: 'sess', afterSeq: 0, latestSeq: 1,
          events: [sessionEvent(1, 'task-a', 'TaskStarted', 'a started')],
        }),
      ),
    )
    render(<SessionEventTimeline sessionId="sess" />)
    const row = await screen.findByTestId('event-row')
    const link = within(row).getByRole('link', { name: 'task-a' })
    expect(link).toHaveAttribute('href', '/tasks/task-a')
  })

  it('dedupes events shared between the initial load and the live stream by session seq', async () => {
    server.use(
      http.get(`${API}/sessions/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'session', streamID: 'sess', afterSeq: 0, latestSeq: 2,
          events: [
            sessionEvent(1, 'task-a', 'TaskStarted', 'a started'),
            sessionEvent(2, 'task-b', 'TaskStarted', 'b started'),
          ],
        }),
      ),
    )
    // Stream replays session seq 2 and adds seq 3.
    streamState.current = {
      events: [
        sessionEvent(2, 'task-b', 'TaskStarted', 'b started'),
        sessionEvent(3, 'task-b', 'ToolCallStarted', 'b tool'),
      ],
      lastSeq: 3,
      status: 'streaming',
    }
    render(<SessionEventTimeline sessionId="sess" />)
    await waitFor(() => expect(screen.getByText('a started')).toBeInTheDocument())
    expect(screen.getByText('b tool')).toBeInTheDocument()
    expect(screen.getAllByTestId('event-row')).toHaveLength(3)
  })

  it('does not stop following when an individual task reaches a terminal event', async () => {
    server.use(
      http.get(`${API}/sessions/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'session', streamID: 'sess', afterSeq: 0, latestSeq: 2,
          events: [
            sessionEvent(1, 'task-a', 'TaskStarted', 'a started'),
            sessionEvent(2, 'task-a', 'TaskSucceeded', 'a done'),
          ],
        }),
      ),
    )
    // Stream is still live even though task-a finished.
    streamState.current = { events: [], lastSeq: 2, status: 'streaming' }
    render(<SessionEventTimeline sessionId="sess" />)
    await waitFor(() => expect(screen.getByText('a done')).toBeInTheDocument())
    expect(screen.getByText('Live')).toBeInTheDocument()
  })

  it('shows a deleted-session banner on SessionDeleted completion', async () => {
    server.use(
      http.get(`${API}/sessions/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'session', streamID: 'sess', afterSeq: 0, latestSeq: 1,
          events: [sessionEvent(1, 'task-a', 'TaskStarted', 'a started')],
        }),
      ),
    )
    streamState.current = {
      events: [],
      lastSeq: 1,
      status: 'complete',
      streamComplete: { lastSeq: 1, type: 'SessionDeleted' },
    }
    render(<SessionEventTimeline sessionId="sess" />)
    await waitFor(() => expect(screen.getByText(/this session was deleted/i)).toBeInTheDocument())
  })
})
