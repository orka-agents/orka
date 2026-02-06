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
    createFileRoute: (_path: string) => (opts: any) => ({ ...opts, path: _path }),
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Route } from './index'

describe('index route', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('exports Route with component', () => {
    expect(Route).toBeDefined()
    expect(Route.component).toBeDefined()
    expect(Route.path).toBe('/')
  })

  it('renders Overview component', () => {
    const IndexComponent = Route.component!
    render(<IndexComponent />)
    expect(screen.getByText('Dashboard')).toBeInTheDocument()
  })
})
