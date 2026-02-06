import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockNavigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => mockNavigate,
    useLocation: () => ({ pathname: '/sessions/test-session' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { render, screen, waitFor } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { SessionDetail } from './session-detail'

describe('SessionDetail', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    mockNavigate.mockClear()
  })

  it('shows skeletons while loading', () => {
    const { container } = render(<SessionDetail sessionId="test-session" />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('shows "Session not found" when API returns 404', async () => {
    server.use(
      http.get('/api/v1/sessions/:id', () => {
        return new HttpResponse(null, { status: 404 })
      })
    )

    render(<SessionDetail sessionId="missing-session" />)
    await waitFor(() => {
      expect(screen.getByText('Session not found')).toBeInTheDocument()
    })
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

  it('shows delete button', async () => {
    render(<SessionDetail sessionId="test-session" />)
    await waitFor(() => {
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })
  })

  it('delete button removes session and navigates', async () => {
    const user = userEvent.setup()
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
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledWith({ to: '/sessions' })
    })
  })
})
