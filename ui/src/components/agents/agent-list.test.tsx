import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/agents' }),
  }
})

import { render, screen, waitFor } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { AgentList } from './agent-list'

describe('AgentList', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('loading state shows skeleton cards', () => {
    const { container } = render(<AgentList />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('empty state shows "No agents registered."', async () => {
    render(<AgentList />)
    await waitFor(() => {
      expect(screen.getByText('No agents registered.')).toBeInTheDocument()
    })
  })

  it('populated grid shows agent cards', async () => {
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'agent-a', namespace: 'default', uid: 'uid-a' },
              spec: {
                model: { provider: 'anthropic', name: 'claude-sonnet-4-20250514' },
                tools: [{ name: 'tool1', enabled: true }],
              },
              status: { activeTasks: 3 },
            },
            {
              metadata: { name: 'agent-b', namespace: 'default', uid: 'uid-b' },
              spec: { runtime: { type: 'copilot' } },
              status: { activeTasks: 0 },
            },
          ],
          metadata: {},
        })
      )
    )

    render(<AgentList />)

    await waitFor(() => {
      expect(screen.getByText('agent-a')).toBeInTheDocument()
    })

    // Agent A: name, namespace, provider badge, model badge, tools count
    expect(screen.getByText('anthropic')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet-4-20250514')).toBeInTheDocument()
    expect(screen.getByText('1 tools')).toBeInTheDocument()
    expect(screen.getByText('Active: 3')).toBeInTheDocument()

    // Agent B: name, runtime badge
    expect(screen.getByText('agent-b')).toBeInTheDocument()
    expect(screen.getByText('copilot runtime')).toBeInTheDocument()
  })

  it('agent card shows namespace', async () => {
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'my-agent', namespace: 'production', uid: 'uid-1' },
              spec: {},
              status: { activeTasks: 0 },
            },
          ],
          metadata: {},
        })
      )
    )

    render(<AgentList />)
    await waitFor(() => {
      expect(screen.getByText('my-agent')).toBeInTheDocument()
    })
    expect(screen.getByText('production')).toBeInTheDocument()
  })

  it('"New Agent" button links to /agents/new', () => {
    render(<AgentList />)
    const link = screen.getByText('New Agent').closest('a')
    expect(link).toHaveAttribute('href', '/agents/new')
  })
})
