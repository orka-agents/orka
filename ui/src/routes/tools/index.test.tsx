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
      useParams: () => ({ toolName: 'test-tool' }),
    }),
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/tools' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Route as ToolIndexRoute } from './index'
import { Route as ToolDetailRoute } from './$toolName'

describe('tools routes', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  describe('tools/index', () => {
    it('exports Route with component', () => {
      expect(ToolIndexRoute).toBeDefined()
      expect(ToolIndexRoute.component).toBeDefined()
      expect(ToolIndexRoute.path).toBe('/tools/')
    })

    it('renders ToolList component', () => {
      const Component = ToolIndexRoute.component!
      render(<Component />)
      expect(screen.getByText('Tools')).toBeInTheDocument()
      expect(screen.getByText('Available tools for AI agents')).toBeInTheDocument()
    })
  })

  describe('tools/$toolName', () => {
    it('exports Route with component', () => {
      expect(ToolDetailRoute).toBeDefined()
      expect(ToolDetailRoute.component).toBeDefined()
      expect(ToolDetailRoute.path).toBe('/tools/$toolName')
    })

    it('renders ToolDetail with toolName param', () => {
      const Component = ToolDetailRoute.component!
      render(<Component />)
      // In loading state, shows skeleton
      expect(document.querySelector('.animate-pulse')).toBeInTheDocument()
    })
  })
})
