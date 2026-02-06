import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

const mockNavigate = vi.fn()
let mockPathname = '/'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createRootRoute: (opts: any) => opts,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => mockNavigate,
    useLocation: () => ({ pathname: mockPathname }),
    Outlet: () => <div data-testid="outlet" />,
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Route } from './__root'

describe('__root route', () => {
  beforeEach(() => {
    mockNavigate.mockClear()
    mockPathname = '/'
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    // jsdom doesn't implement matchMedia
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation((query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    })
  })

  it('exports a Route with a component', () => {
    expect(Route).toBeDefined()
    expect(Route.component).toBeDefined()
  })

  it('renders layout when authenticated', () => {
    const RootComponent = Route.component!
    render(<RootComponent />)
    expect(screen.getByText('Dashboard')).toBeInTheDocument()
    expect(screen.getByTestId('outlet')).toBeInTheDocument()
  })

  it('navigates to /login when no token and pathname is not /login', () => {
    useAuthStore.setState({ token: null })
    mockPathname = '/tasks'
    const RootComponent = Route.component!
    render(<RootComponent />)
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/login' })
  })

  it('renders Outlet on /login path without token', () => {
    useAuthStore.setState({ token: null })
    mockPathname = '/login'
    const RootComponent = Route.component!
    render(<RootComponent />)
    expect(screen.getByTestId('outlet')).toBeInTheDocument()
    expect(mockNavigate).not.toHaveBeenCalled()
  })

  it('renders null when no token and not on login path', () => {
    useAuthStore.setState({ token: null })
    mockPathname = '/tasks'
    const RootComponent = Route.component!
    const { container } = render(<RootComponent />)
    // navigates away, renders null
    expect(container.querySelector('[data-testid="outlet"]')).not.toBeInTheDocument()
  })
})
