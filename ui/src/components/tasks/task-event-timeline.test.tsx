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
    streamState.current = {
      events: [makeEvent({ seq: 2, type: 'TaskStarted' }), makeEvent({ seq: 3, type: 'ToolCallStarted' })],
      lastSeq: 3,
      status: 'streaming',
    }
    render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(screen.getByText('TaskCreated')).toBeInTheDocument())
    expect(screen.getByText('ToolCallStarted')).toBeInTheDocument()
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
    server.use(
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 500,
          events: [makeEvent({ seq: 99, type: 'TaskStarted' }), makeEvent({ seq: 100, type: 'ModelMessage' })],
        }),
      ),
    )
    render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(streamCalls.some((c) => c.enabled && c.after === 100)).toBe(true))
    expect(streamCalls.some((c) => c.enabled && c.after === 500)).toBe(false)
  })

  it('backfills a completed task with more events than the loaded page, even when not following', async () => {
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
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
  })

  it('closes the backfill stream once it has caught up so pausing follow works', async () => {
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
    render(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByText('caught up')).toBeInTheDocument())
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
    const view = render(<TaskEventTimeline taskId="tk" taskPhase={undefined} />)
    await waitFor(() => expect(screen.getByText('TaskCreated')).toBeInTheDocument())
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
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
    streamState.current = { events: [], lastSeq: 5, status: 'streaming' }
    const view = render(<TaskEventTimeline taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(screen.getByText('mid run')).toBeInTheDocument())
    expect(streamCalls[streamCalls.length - 1].enabled).toBe(true)

    streamCalls.length = 0
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(streamCalls.length).toBeGreaterThan(0))
    expect(streamCalls[streamCalls.length - 1].enabled).toBe(true)

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
    await user.click(screen.getByRole('button', { name: /stop following/i }))
    streamCalls.length = 0
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(streamCalls.length).toBeGreaterThan(0))
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
  })

  it('does not open a stream for an already-terminal task opened fresh with no events', async () => {
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
    await new Promise((r) => setTimeout(r, 50))
    expect(streamCalls.every((c) => !c.enabled)).toBe(true)
  })

  it('keeps retrying terminal catch-up across the controller race until the terminal event lands', async () => {
    let getCalls = 0
    server.use(
      http.get(`${API}/tasks/:id/events`, () => {
        getCalls += 1
        if (getCalls <= 2) {
          return HttpResponse.json({
            namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 0, events: [],
          })
        }
        return HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 2,
          events: [
            makeEvent({ seq: 1, type: 'TaskStarted', summary: 'started' }),
            makeEvent({ seq: 2, type: 'TaskSucceeded', summary: 'finished after race' }),
          ],
        })
      }),
    )
    const view = render(<TaskEventTimeline taskId="tk" taskPhase={undefined} />)
    await waitFor(() => expect(getCalls).toBe(1))
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByText('finished after race')).toBeInTheDocument(), {
      timeout: 4000,
    })
    expect(getCalls).toBeGreaterThanOrEqual(3)
  }, 8000)

  it('stops terminal catch-up after a bounded number of attempts when no terminal event ever lands', async () => {
    let getCalls = 0
    server.use(
      http.get(`${API}/tasks/:id/events`, () => {
        getCalls += 1
        return HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'tk', afterSeq: 0, latestSeq: 0, events: [],
        })
      }),
    )
    const view = render(<TaskEventTimeline taskId="tk" taskPhase={undefined} />)
    await waitFor(() => expect(getCalls).toBe(1))
    view.rerender(<TaskEventTimeline taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(getCalls).toBe(6), { timeout: 6000 })
    await new Promise((r) => setTimeout(r, 1500))
    expect(getCalls).toBe(6)
  }, 12000)
})
