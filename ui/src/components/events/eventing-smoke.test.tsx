import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen, waitFor, within } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { makeEvent } from '@/test/fixtures/events'
import { makeTrace } from '@/test/fixtures/trace'
import type { UseExecutionEventStreamResult } from '@/hooks/use-execution-event-stream'

// End-to-end UI smoke for the evented-execution surfaces. One mocked backend,
// exercised through the real task/session detail components and their tabs.

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockNavigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => {
      let href = to
      if (typeof to === 'string' && params) {
        for (const [k, v] of Object.entries(params)) href = href.replace(`$${k}`, v as string)
      }
      return <a href={href} {...props}>{children}</a>
    },
    useNavigate: () => mockNavigate,
    useLocation: () => ({ pathname: '/tasks/smoke-task' }),
  }
})

// Deterministic stream output, swappable per test.
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

import { TaskDetail } from '@/components/tasks/task-detail'
import { SessionDetail } from '@/components/sessions/session-detail'

const API = '/api/v1'

function mockTask(overrides: Record<string, unknown> = {}) {
  return {
    metadata: { name: 'smoke-task', namespace: 'default', uid: 'uid-smoke', creationTimestamp: '2026-06-13T00:00:00Z' },
    spec: { type: 'agent', agentRef: { name: 'planner' }, prompt: 'do work' },
    status: { phase: 'Succeeded' },
    ...overrides,
  }
}

describe('evented execution UI smoke', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    mockNavigate.mockClear()
    streamState.current = {}
  })

  it('walks the task timeline, trace, approval, and fork flows end to end', async () => {
    const user = userEvent.setup()
    // The approval flips to approved once a decision is posted, like the backend.
    let approvalDecided = false
    server.use(
      http.get(`${API}/tasks/:id`, () => HttpResponse.json(mockTask())),
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'smoke-task', afterSeq: 0, latestSeq: 3,
          events: [
            makeEvent({ seq: 1, type: 'TaskStarted', summary: 'task started' }),
            makeEvent({ seq: 2, type: 'ToolCallStarted', toolName: 'web_fetch', summary: 'fetching' }),
            makeEvent({ seq: 3, type: 'TaskSucceeded', summary: 'task done' }),
          ],
        }),
      ),
      http.get(`${API}/tasks/:id/trace`, () =>
        HttpResponse.json(
          makeTrace({
            task: { namespace: 'default', name: 'smoke-task', phase: 'Succeeded', resultAvailable: true },
            latestSeq: 3,
            modelRequests: [{ id: 'm1', status: 'completed', startSeq: 1, endSeq: 2, summary: 'one model call' }],
            toolCalls: [{ id: 't1', name: 'web_fetch', status: 'completed', startSeq: 2, endSeq: 3 }],
          }),
        ),
      ),
      http.get(`${API}/tasks/:id/approvals`, () =>
        HttpResponse.json({
          namespace: 'default', taskName: 'smoke-task',
          approvals: [{ id: 'ap-1', action: 'web_fetch', riskSummary: 'fetch external URL', status: approvalDecided ? 'approved' : 'pending', createdAt: '2026-06-13T00:00:00Z', decisionActor: approvalDecided ? 'tester' : undefined }],
        }),
      ),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, () => {
        approvalDecided = true
        return HttpResponse.json({ id: 'ap-1', action: 'web_fetch', status: 'approved', decisionActor: 'tester' })
      }),
      http.post(`${API}/tasks/:id/fork`, () =>
        HttpResponse.json(
          {
            namespace: 'default', sourceTaskName: 'smoke-task', newTaskName: 'smoke-task-fork-1234', afterSeq: 2,
            forkContext: { sourceNamespace: 'default', sourceTask: 'smoke-task', afterSeq: 2, events: [], truncated: false },
          },
          { status: 201 },
        ),
      ),
    )

    render(<TaskDetail taskId="smoke-task" />)
    await waitFor(() => expect(screen.getByText('smoke-task')).toBeInTheDocument())

    // 1. Timeline tab shows the task's events.
    await user.click(screen.getByRole('tab', { name: 'Timeline' }))
    await waitFor(() => expect(screen.getByText('task started')).toBeInTheDocument())
    expect(screen.getByText('task done')).toBeInTheDocument()
    expect(screen.getAllByTestId('event-row')).toHaveLength(3)

    // 2. Trace tab explains model + tool sections.
    await user.click(screen.getByRole('tab', { name: 'Trace' }))
    await waitFor(() => expect(screen.getByText('Execution trace')).toBeInTheDocument())
    expect(screen.getByText('Model requests')).toBeInTheDocument()
    expect(screen.getByText('one model call')).toBeInTheDocument()
    expect(screen.getByText('Tool calls')).toBeInTheDocument()

    // 3. Approvals tab: approve the pending request.
    await user.click(screen.getByRole('tab', { name: 'Approvals' }))
    await waitFor(() => expect(screen.getByText('fetch external URL')).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument())

    // 4. Fork from an event row on the Timeline tab.
    await user.click(screen.getByRole('tab', { name: 'Timeline' }))
    await waitFor(() => expect(screen.getAllByRole('button', { name: /fork from here/i }).length).toBeGreaterThan(0))
    await user.click(screen.getAllByRole('button', { name: /fork from here/i })[1])
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /create fork/i }))
    await waitFor(() =>
      expect(within(dialog).getByRole('link', { name: /smoke-task-fork-1234/ })).toBeInTheDocument(),
    )
  })

  it('recovers task history after a remount (refresh) without a live stream', async () => {
    server.use(
      http.get(`${API}/tasks/:id`, () => HttpResponse.json(mockTask({ status: { phase: 'Running' } }))),
      http.get(`${API}/tasks/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'smoke-task', afterSeq: 0, latestSeq: 2,
          events: [
            makeEvent({ seq: 1, type: 'TaskStarted', summary: 'task started' }),
            makeEvent({ seq: 2, type: 'WorkerStarted', summary: 'worker up' }),
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    const view = render(<TaskDetail taskId="smoke-task" />)
    await waitFor(() => expect(screen.getByText('smoke-task')).toBeInTheDocument())
    await user.click(screen.getByRole('tab', { name: 'Timeline' }))
    await waitFor(() => expect(screen.getByText('task started')).toBeInTheDocument())

    // Simulate a browser refresh: unmount and remount; history reloads from the API.
    view.unmount()
    render(<TaskDetail taskId="smoke-task" />)
    await waitFor(() => expect(screen.getByText('smoke-task')).toBeInTheDocument())
    await user.click(screen.getByRole('tab', { name: 'Timeline' }))
    await waitFor(() => expect(screen.getByText('task started')).toBeInTheDocument())
    expect(screen.getByText('worker up')).toBeInTheDocument()
  })

  it('shows a multi-task session timeline with task links', async () => {
    server.use(
      http.get(`${API}/sessions/:id`, () =>
        HttpResponse.json({ name: 'smoke-session', namespace: 'default', messageCount: '4' }),
      ),
      http.get(`${API}/sessions/:id/events`, () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'session', streamID: 'smoke-session', afterSeq: 0, latestSeq: 3,
          events: [
            makeEvent({ seq: 1, streamType: 'session', streamID: 'smoke-session', taskName: 'task-a', type: 'TaskStarted', summary: 'a started' }),
            makeEvent({ seq: 2, streamType: 'session', streamID: 'smoke-session', taskName: 'task-b', type: 'TaskStarted', summary: 'b started' }),
            makeEvent({ seq: 3, streamType: 'session', streamID: 'smoke-session', taskName: 'task-a', type: 'TaskSucceeded', summary: 'a done' }),
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    render(<SessionDetail sessionId="smoke-session" />)
    await waitFor(() => expect(screen.getByText('smoke-session')).toBeInTheDocument())
    await user.click(screen.getByRole('tab', { name: 'Timeline' }))
    await waitFor(() => expect(screen.getByText('a started')).toBeInTheDocument())
    expect(screen.getByText('b started')).toBeInTheDocument()
    // Task identities link to their detail pages.
    const links = screen.getAllByRole('link', { name: 'task-a' })
    expect(links[0]).toHaveAttribute('href', '/tasks/task-a')
    expect(screen.getByRole('link', { name: 'task-b' })).toBeInTheDocument()
  })
})
