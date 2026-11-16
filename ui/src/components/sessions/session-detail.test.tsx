import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockNavigate = vi.fn()
const mockSearch: { current: { tab?: string } } = { current: {} }
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => mockNavigate,
    useSearch: () => mockSearch.current,
    useLocation: () => ({ pathname: '/sessions/test-session' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

// The session timeline tab opens a live SSE stream; stub the stream hook so this
// suite stays focused on the detail layout and tab wiring.
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
  }),
}))

import { render, screen, waitFor } from '@/test/test-utils'
import { toast } from 'sonner'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { SessionDetail } from './session-detail'

describe('SessionDetail', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    mockNavigate.mockClear()
    mockSearch.current = {}
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
    vi.restoreAllMocks()
  })

  it('shows skeletons while loading', () => {
    const { container } = render(<SessionDetail sessionId="test-session" />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('shows a styled not-found page with a way back when the API returns 404', async () => {
    let eventRequests = 0
    server.use(
      http.get('/api/v1/sessions/:id', () => {
        return new HttpResponse(null, { status: 404 })
      }),
      http.get('/api/v1/sessions/:id/events', () => {
        eventRequests += 1
        return HttpResponse.json({ events: [], latestSeq: 0 })
      }),
    )

    render(<SessionDetail sessionId="missing-session" />)
    expect(await screen.findByRole('heading', { level: 1, name: 'Session not found' })).toBeInTheDocument()
    expect(screen.getByText('No session named "missing-session"')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to sessions' })).toHaveAttribute('href', '/sessions')
    expect(document.title).toBe('Session not found · Orka')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: 'Timeline' })).not.toBeInTheDocument()
    expect(eventRequests).toBe(0)
  })

  it('shows a load error, not "Session not found", for other failures', async () => {
    server.use(
      // A client error, so useSession does not retry it.
      http.get('/api/v1/sessions/:id', () =>
        HttpResponse.json({ error: { code: 400, message: 'invalid session name' } }, { status: 400 }),
      ),
    )

    render(<SessionDetail sessionId="broken-session" />)
    expect(await screen.findByRole('alert')).toHaveTextContent('invalid session name')
    expect(screen.getByRole('heading', { level: 1, name: 'Could not load session' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to sessions' })).toHaveAttribute('href', '/sessions')
    expect(screen.queryByText('Session not found')).not.toBeInTheDocument()
  })

  it('shows a not-authorized message (not "Session not found") on 403', async () => {
    server.use(
      http.get('/api/v1/sessions/:id', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      )
    )

    render(<SessionDetail sessionId="secret-session" />)
    await waitFor(() => {
      expect(screen.getByText('Not authorized to view sessions')).toBeInTheDocument()
    })
    expect(screen.getByText(/read permission \(not authorized\)/)).toBeInTheDocument()
    expect(screen.getByRole('alert')).toBeInTheDocument()
    expect(screen.queryByText('Session not found')).not.toBeInTheDocument()
  })

  it('names the back link to the session list and nests no button in it', async () => {
    server.use(
      http.get('/api/v1/sessions/:id', () => HttpResponse.json({ name: 'back-session', namespace: 'default' })),
    )
    render(<SessionDetail sessionId="back-session" />)
    const back = await screen.findByRole('link', { name: 'Back to sessions' })
    expect(back).toHaveAttribute('href', '/sessions')
    expect(back.querySelector('button')).toBeNull()
    expect(back.closest('button')).toBeNull()
  })

  it('shows session name and namespace', async () => {
    server.use(
      http.get('/api/v1/sessions/:id', () => {
        return HttpResponse.json({
          name: 'my-session',
          namespace: 'prod',
          messageCount: '10',
          inputTokens: '200',
          outputTokens: '400',
        })
      })
    )

    render(<SessionDetail sessionId="my-session" />)
    await waitFor(() => {
      expect(screen.getByText('my-session')).toBeInTheDocument()
    })
    expect(screen.getByText('prod')).toBeInTheDocument()
  })

  it('shows stats cards with messages and tokens', async () => {
    server.use(
      http.get('/api/v1/sessions/:id', () => {
        return HttpResponse.json({
          name: 'stat-session',
          namespace: 'default',
          messageCount: '42',
          inputTokens: '1500',
          outputTokens: '3000',
          activeTask: 'running-task',
        })
      })
    )

    render(<SessionDetail sessionId="stat-session" />)
    await waitFor(() => {
      expect(screen.getByText('42')).toBeInTheDocument()
    })
    expect(screen.getByText('1500')).toBeInTheDocument()
    expect(screen.getByText('3000')).toBeInTheDocument()
    expect(screen.getByText('running-task')).toBeInTheDocument()
  })

  it('shows stats card titles', async () => {
    render(<SessionDetail sessionId="test-session" />)
    await waitFor(() => {
      expect(screen.getByText('Messages')).toBeInTheDocument()
    })
    expect(screen.getByText('Input Tokens')).toBeInTheDocument()
    expect(screen.getByText('Output Tokens')).toBeInTheDocument()
    expect(screen.getByText('Active Task')).toBeInTheDocument()
  })

  it('shows transcript section', async () => {
    render(<SessionDetail sessionId="test-session" />)
    await waitFor(() => {
      expect(screen.getByText('Transcript')).toBeInTheDocument()
    })
  })

  it('exposes a Timeline tab that renders the session event timeline', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/sessions/:id/events', () =>
        HttpResponse.json({
          namespace: 'default', streamType: 'session', streamID: 'tl-session', afterSeq: 0, latestSeq: 1,
          events: [
            {
              id: 'e1', namespace: 'default', streamType: 'session', streamID: 'tl-session', seq: 1,
              type: 'TaskStarted', severity: 'info', createdAt: '2026-06-13T00:00:00Z',
              taskName: 'task-x', summary: 'x started',
            },
          ],
        }),
      ),
    )
    render(<SessionDetail sessionId="tl-session" />)
    await waitFor(() => expect(screen.getByRole('tab', { name: 'Timeline' })).toBeInTheDocument())
    await user.click(screen.getByRole('tab', { name: 'Timeline' }))
    await waitFor(() => expect(screen.getByText('Session timeline')).toBeInTheDocument())
    expect(await screen.findByText('x started')).toBeInTheDocument()
  })

  it('shows delete button', async () => {
    render(<SessionDetail sessionId="test-session" />)
    await waitFor(() => {
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })
  })

  it('delete button removes a confirmed session and navigates', async () => {
    const user = userEvent.setup()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    server.use(
      http.get('/api/v1/sessions/:id', () =>
        HttpResponse.json({
          name: 'del-session',
          namespace: 'default',
          messageCount: '3',
          inputTokens: '100',
          outputTokens: '200',
        }),
      ),
    )
    render(<SessionDetail sessionId="del-session" />)
    await waitFor(() => {
      expect(screen.getByText('del-session')).toBeInTheDocument()
    })
    await user.click(screen.getByRole('button', { name: /delete/i }))
    expect(window.confirm).toHaveBeenCalledWith('Delete session "del-session"?')
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledWith({ to: '/sessions' })
    })
    expect(toast.success).toHaveBeenCalledWith('Session deleted')
  })

  it('declining the confirmation keeps the session', async () => {
    const user = userEvent.setup()
    let deletes = 0
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    server.use(
      http.get('/api/v1/sessions/:id', () => HttpResponse.json({ name: 'keep-session', namespace: 'default' })),
      http.delete('/api/v1/sessions/:id', () => {
        deletes += 1
        return new HttpResponse(null, { status: 204 })
      }),
    )
    render(<SessionDetail sessionId="keep-session" />)
    await user.click(await screen.findByRole('button', { name: /delete/i }))
    expect(deletes).toBe(0)
    expect(mockNavigate).not.toHaveBeenCalled()
  })

  it('shows the server message instead of throwing when a viewer deletes', async () => {
    const user = userEvent.setup()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    server.use(
      http.get('/api/v1/sessions/:id', () => HttpResponse.json({ name: 'ro-session', namespace: 'default' })),
      http.delete('/api/v1/sessions/:id', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      ),
    )
    render(<SessionDetail sessionId="ro-session" />)
    await user.click(await screen.findByRole('button', { name: /delete/i }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete session: not authorized'))
    expect(mockNavigate).not.toHaveBeenCalled()
  })

  it('keeps keyboard focus on Delete while a refused delete is in flight', async () => {
    const user = userEvent.setup()
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    let deletes = 0
    server.use(
      http.get('/api/v1/sessions/:id', () => HttpResponse.json({ name: 'ro-session', namespace: 'default' })),
      http.delete('/api/v1/sessions/:id', async () => {
        deletes += 1
        await gate
        return HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })
      }),
    )
    render(<SessionDetail sessionId="ro-session" />)
    const button = await screen.findByRole('button', { name: /delete/i })
    button.focus()
    await user.keyboard('{Enter}')

    // Pending: marked unavailable but never disabled, so focus stays put.
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
    expect(button).not.toBeDisabled()
    expect(button).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(deletes).toBe(1)

    release()
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete session: not authorized'))
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled'))
    expect(button).toHaveFocus()
    expect(mockNavigate).not.toHaveBeenCalled()
  })

  it('opens the tab named in the URL and records tab changes there', async () => {
    const user = userEvent.setup()
    mockSearch.current = { tab: 'timeline' }
    server.use(
      http.get('/api/v1/sessions/:id', () => HttpResponse.json({ name: 'tab-session', namespace: 'default' })),
    )
    render(<SessionDetail sessionId="tab-session" />)
    await waitFor(() => expect(screen.getByRole('tab', { name: 'Timeline' })).toHaveAttribute('aria-selected', 'true'))
    await user.click(screen.getByRole('tab', { name: 'Overview' }))
    expect(mockNavigate).toHaveBeenCalledWith({
      to: '/sessions/$sessionId',
      params: { sessionId: 'tab-session' },
      search: { tab: 'overview' },
      replace: true,
    })
    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('aria-selected', 'true')
  })

  it('falls back to Overview for an unknown tab in the URL', async () => {
    mockSearch.current = { tab: 'bogus' }
    server.use(
      http.get('/api/v1/sessions/:id', () => HttpResponse.json({ name: 'tab-session', namespace: 'default' })),
    )
    render(<SessionDetail sessionId="tab-session" />)
    await waitFor(() => expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('aria-selected', 'true'))
  })
})
