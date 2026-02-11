import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/live' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { AgentGridView } from './agent-grid-view'

describe('AgentGridView', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows loading skeletons', () => {
    server.use(http.get('/api/v1/tasks', async () => {
      await new Promise(r => setTimeout(r, 5000))
      return HttpResponse.json({ items: [], metadata: {} })
    }))
    const { container } = render(<AgentGridView />)
    expect(container.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0)
  })

  it('shows empty state when no running tasks', async () => {
    render(<AgentGridView />)
    await waitFor(() => {
      expect(screen.getByText(/No tasks currently running/)).toBeInTheDocument()
    })
  })

  it('shows running tasks as cards', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [
        { metadata: { name: 'running-1', namespace: 'default', uid: 'u1' }, spec: { type: 'agent', agentRef: { name: 'my-agent' } }, status: { phase: 'Running', startTime: new Date().toISOString() } },
        { metadata: { name: 'succeeded-1', namespace: 'default', uid: 'u2' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } },
      ],
      metadata: {},
    })))
    render(<AgentGridView />)
    await waitFor(() => {
      expect(screen.getByText('running-1')).toBeInTheDocument()
    })
    expect(screen.queryByText('succeeded-1')).not.toBeInTheDocument()
    expect(screen.getByText('my-agent')).toBeInTheDocument()
  })

  it('shows correct count', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [
        { metadata: { name: 'r1', namespace: 'default', uid: 'u1' }, spec: { type: 'agent' }, status: { phase: 'Running', startTime: new Date().toISOString() } },
        { metadata: { name: 'r2', namespace: 'default', uid: 'u2' }, spec: { type: 'ai' }, status: { phase: 'Running', startTime: new Date().toISOString() } },
      ],
      metadata: {},
    })))
    render(<AgentGridView />)
    await waitFor(() => {
      expect(screen.getByText('2 active tasks')).toBeInTheDocument()
    })
  })
})
