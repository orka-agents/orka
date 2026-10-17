import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

const mockNavigate = vi.fn()
let mockPathname = '/'
let mockSearchStr = ''

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createRootRoute: (opts: any) => opts,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => mockNavigate,
    // Like the browser-history router, the fragment mirrors the address bar.
    useLocation: () => ({ pathname: mockPathname, searchStr: mockSearchStr, hash: window.location.hash.slice(1) }),
    Outlet: () => <div data-testid="outlet" />,
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { captureHashToken, takeHashToken } from '@/lib/login-return'
import { Route } from './__root'

describe('__root route', () => {
  beforeEach(() => {
    mockNavigate.mockClear()
    mockPathname = '/'
    mockSearchStr = ''
    sessionStorage.clear()
    takeHashToken()
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

  it('applies the system theme while the user follows it, and an explicit pick over it', () => {
    vi.mocked(window.matchMedia).mockImplementation((query: string) => ({
      matches: query === '(prefers-color-scheme: dark)',
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    }))
    const RootComponent = Route.component!
    useUIStore.setState({ theme: 'system' })
    const { unmount } = render(<RootComponent />)
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    unmount()

    useUIStore.setState({ theme: 'light' })
    render(<RootComponent />)
    expect(document.documentElement.classList.contains('dark')).toBe(false)
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

  it('keeps a consent callback through sign-in', () => {
    useAuthStore.setState({ token: null })
    mockPathname = '/settings/connectors'
    mockSearchStr = '?status=pending&connection=github-1'
    window.history.replaceState(null, '', '/settings/connectors?status=pending&connection=github-1#completion=one-time')
    const RootComponent = Route.component!
    render(<RootComponent />)
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/login', hash: 'completion=one-time' })
    expect(sessionStorage.getItem('orka-login-return')).toBe('/settings/connectors?status=pending&connection=github-1')
    window.history.replaceState(null, '', '/')
  })

  it('sends a deep-link #token= to the login page without the token in the address', () => {
    useAuthStore.setState({ token: null })
    window.history.replaceState(null, '', '/monitors/example?tab=runs#token=deep-link-token')
    captureHashToken()
    mockPathname = '/monitors/example'
    mockSearchStr = '?tab=runs'
    const RootComponent = Route.component!
    render(<RootComponent />)
    expect(window.location.href).not.toContain('deep-link-token')
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/login' })
    expect(sessionStorage.getItem('orka-login-return')).toBe('/monitors/example?tab=runs')
    window.history.replaceState(null, '', '/')
  })

  it('treats a captured #token= as a sign-in request over an existing token', () => {
    window.history.replaceState(null, '', '/tasks#token=new-token')
    captureHashToken()
    mockPathname = '/tasks'
    const RootComponent = Route.component!
    const { container } = render(<RootComponent />)
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/login' })
    expect(container.querySelector('[data-testid="outlet"]')).not.toBeInTheDocument()
    window.history.replaceState(null, '', '/')
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
