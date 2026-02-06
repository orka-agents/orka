import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createFileRoute: (_path: string) => (opts: any) => ({
      ...opts,
      path: _path,
      useParams: () => ({ agentId: 'test-agent-123' }),
    }),
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/agents' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Route as AgentIndexRoute } from './index'
import { Route as AgentNewRoute } from './new'
import { Route as AgentDetailRoute } from './$agentId'

describe('agents routes', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  describe('agents/index', () => {
    it('exports Route with component', () => {
      expect(AgentIndexRoute).toBeDefined()
      expect(AgentIndexRoute.component).toBeDefined()
      expect(AgentIndexRoute.path).toBe('/agents/')
    })

    it('renders AgentList component', () => {
      const Component = AgentIndexRoute.component!
      render(<Component />)
      expect(screen.getByText('Agents')).toBeInTheDocument()
      expect(screen.getByText('Registered AI agent configurations')).toBeInTheDocument()
    })
  })

  describe('agents/new', () => {
    it('exports Route with component', () => {
      expect(AgentNewRoute).toBeDefined()
      expect(AgentNewRoute.component).toBeDefined()
      expect(AgentNewRoute.path).toBe('/agents/new')
    })

    it('renders AgentCreateForm component', () => {
      const Component = AgentNewRoute.component!
      render(<Component />)
      expect(screen.getByRole('heading', { name: 'Create Agent' })).toBeInTheDocument()
    })
  })

  describe('agents/$agentId', () => {
    it('exports Route with component', () => {
      expect(AgentDetailRoute).toBeDefined()
      expect(AgentDetailRoute.component).toBeDefined()
      expect(AgentDetailRoute.path).toBe('/agents/$agentId')
    })

    it('renders AgentDetail with agentId param', () => {
      const Component = AgentDetailRoute.component!
      render(<Component />)
      // In loading state, shows skeleton
      expect(document.querySelector('.animate-pulse')).toBeInTheDocument()
    })
  })
})
