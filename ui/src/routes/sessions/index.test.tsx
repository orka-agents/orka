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
      useParams: () => ({ sessionId: 'test-session-123' }),
    }),
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/sessions' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Route as SessionIndexRoute } from './index'
import { Route as SessionDetailRoute } from './$sessionId'

describe('sessions routes', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  describe('sessions/index', () => {
    it('exports Route with component', () => {
      expect(SessionIndexRoute).toBeDefined()
      expect(SessionIndexRoute.component).toBeDefined()
      expect(SessionIndexRoute.path).toBe('/sessions/')
    })

    it('renders SessionList component', () => {
      const Component = SessionIndexRoute.component!
      render(<Component />)
      expect(screen.getByText('Sessions')).toBeInTheDocument()
      expect(screen.getByText('View conversation sessions')).toBeInTheDocument()
    })
  })

  describe('sessions/$sessionId', () => {
    it('exports Route with component', () => {
      expect(SessionDetailRoute).toBeDefined()
      expect(SessionDetailRoute.component).toBeDefined()
      expect(SessionDetailRoute.path).toBe('/sessions/$sessionId')
    })

    it('renders SessionDetail with sessionId param', () => {
      const Component = SessionDetailRoute.component!
      render(<Component />)
      // In loading state, shows skeleton
      expect(document.querySelector('.animate-pulse')).toBeInTheDocument()
    })
  })
})
