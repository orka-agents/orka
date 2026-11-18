import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

const mockNavigate = vi.fn()
let mockPathname = '/login'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createFileRoute: (_path: string) => (opts: any) => ({ ...opts, path: _path }),
    useNavigate: () => mockNavigate,
    // Like the browser-history router, the fragment mirrors the address bar.
    useLocation: () => ({ pathname: mockPathname, hash: window.location.hash.slice(1) }),
  }
})

import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { useAuthStore } from '@/stores/auth'
import { captureHashToken, takeHashToken } from '@/lib/login-return'
import { DEMO_TOKEN } from '@/demo/token'
import { Route } from './login'

describe('login route', () => {
  beforeEach(() => {
    mockNavigate.mockClear()
    mockPathname = '/login'
    useAuthStore.setState({ token: null })
    sessionStorage.clear()
    takeHashToken()
  })

  it('exports Route with component', () => {
    expect(Route).toBeDefined()
    expect(Route.component).toBeDefined()
    expect(Route.path).toBe('/login')
  })

  it('renders login form with token input and sign in button', () => {
    const LoginPage = Route.component!
    render(<LoginPage />)
    expect(screen.getByPlaceholderText('Paste your token here...')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /sign in/i })).toBeInTheDocument()
    expect(screen.getByText('Orka')).toBeInTheDocument()
  })

  it('submit button is disabled when input is empty', () => {
    const LoginPage = Route.component!
    render(<LoginPage />)
    const button = screen.getByRole('button', { name: /sign in/i })
    expect(button).toBeDisabled()
  })

  it('submit button is enabled when input has value', () => {
    const LoginPage = Route.component!
    render(<LoginPage />)
    const input = screen.getByPlaceholderText('Paste your token here...')
    fireEvent.change(input, { target: { value: 'my-token' } })
    const button = screen.getByRole('button', { name: /sign in/i })
    expect(button).not.toBeDisabled()
  })

  it('submitting sets token in auth store', async () => {
    const LoginPage = Route.component!
    render(<LoginPage />)
    const input = screen.getByPlaceholderText('Paste your token here...')
    fireEvent.change(input, { target: { value: 'my-secret-token' } })
    fireEvent.click(screen.getByRole('button', { name: /sign in/i }))
    await waitFor(() => {
      expect(useAuthStore.getState().token).toBe('my-secret-token')
    })
  })

  it('navigates to / when already authenticated', () => {
    useAuthStore.setState({ token: 'existing-token' })
    const LoginPage = Route.component!
    render(<LoginPage />)
    expect(mockNavigate).toHaveBeenCalledWith({ href: '/' })
  })

  it('returns to the page that sent the visitor here, fragment included', () => {
    sessionStorage.setItem('orka-login-return', '/settings/connectors?status=pending&connection=github-1')
    window.history.replaceState(null, '', '/login#completion=one-time')
    useAuthStore.setState({ token: 'existing-token' })
    const LoginPage = Route.component!
    render(<LoginPage />)
    expect(mockNavigate).toHaveBeenCalledWith({ href: '/settings/connectors?status=pending&connection=github-1#completion=one-time' })
    expect(sessionStorage.getItem('orka-login-return')).toBeNull()
    window.history.replaceState(null, '', '/')
  })

  it('sends the visitor on once when signing in remounts it under the destination address', () => {
    sessionStorage.setItem('orka-login-return', '/settings/connectors?status=pending&connection=github-1')
    window.history.replaceState(null, '', '/login#completion=one-time')
    useAuthStore.setState({ token: 'existing-token' })
    const LoginPage = Route.component!
    render(<LoginPage />)
    // The token swaps the root layout before the route match changes, so
    // the login page mounts again for a moment at the new address.
    mockPathname = '/settings/connectors'
    render(<LoginPage />)
    expect(mockNavigate).toHaveBeenCalledTimes(1)
    expect(mockNavigate).toHaveBeenCalledWith({ href: '/settings/connectors?status=pending&connection=github-1#completion=one-time' })
    window.history.replaceState(null, '', '/')
  })

  it('handles #token=... hash fragment from CLI login', async () => {
    window.history.replaceState(null, '', '/login#token=hash-cli-token')
    captureHashToken()
    const LoginPage = Route.component!
    render(<LoginPage />)
    await waitFor(() => {
      expect(useAuthStore.getState().token).toBe('hash-cli-token')
    })
    expect(window.location.hash).toBe('')
    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith({ href: '/' }))
    window.history.replaceState(null, '', '/')
  })

  it('signs in with a deep-link token and returns to the deep link', async () => {
    let authorization: string | null = null
    server.use(http.get('/api/v1/auth/validate', ({ request }) => {
      authorization = request.headers.get('Authorization')
      return new HttpResponse(null, { status: 200 })
    }))
    // What the root route leaves behind for /monitors/example#token=deep-link-token.
    window.history.replaceState(null, '', '/monitors/example#token=deep-link-token')
    captureHashToken()
    sessionStorage.setItem('orka-login-return', '/monitors/example')
    window.history.replaceState(null, '', '/login')

    const LoginPage = Route.component!
    render(<LoginPage />)
    expect(screen.getByRole('button', { name: /validating/i })).toBeDisabled()

    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith({ href: '/monitors/example' }))
    expect(mockNavigate).toHaveBeenCalledTimes(1)
    expect(authorization).toBe('Bearer deep-link-token')
    expect(useAuthStore.getState().token).toBe('deep-link-token')
    window.history.replaceState(null, '', '/')
  })

  it('validates a captured token before leaving, even over an existing token', async () => {
    let release: () => void = () => {}
    server.use(http.get('/api/v1/auth/validate', () => new Promise<Response>((resolve) => {
      release = () => resolve(new HttpResponse(null, { status: 200 }))
    })))
    useAuthStore.setState({ token: 'old-token' })
    window.history.replaceState(null, '', '/login#token=new-token')
    captureHashToken()

    const LoginPage = Route.component!
    render(<LoginPage />)
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(mockNavigate).not.toHaveBeenCalled()

    release()
    await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith({ href: '/' }))
    expect(useAuthStore.getState().token).toBe('new-token')
    window.history.replaceState(null, '', '/')
  })

  it('stays on the error when a captured token is rejected while an earlier one is stored', async () => {
    server.use(http.get('/api/v1/auth/validate', () => new HttpResponse(null, { status: 401 })))
    useAuthStore.setState({ token: 'old-token' })
    window.history.replaceState(null, '', '/login#token=expired-token')
    captureHashToken()

    const LoginPage = Route.component!
    render(<LoginPage />)

    expect(await screen.findByText('Token from CLI is invalid or expired. Please generate a new one.')).toBeInTheDocument()
    // The rejected switch must not continue silently under the old identity.
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(mockNavigate).not.toHaveBeenCalled()
    expect(useAuthStore.getState().token).toBe('old-token')
    window.history.replaceState(null, '', '/')
  })

  it('reports a captured token the server rejects', async () => {
    server.use(http.get('/api/v1/auth/validate', () => new HttpResponse(null, { status: 401 })))
    window.history.replaceState(null, '', '/login#token=expired-token')
    captureHashToken()

    const LoginPage = Route.component!
    render(<LoginPage />)

    expect(await screen.findByText('Token from CLI is invalid or expired. Please generate a new one.')).toBeInTheDocument()
    expect(useAuthStore.getState().token).toBeNull()
    expect(mockNavigate).not.toHaveBeenCalled()
    window.history.replaceState(null, '', '/')
  })

  it('signs in to the demo build without asking for a token', async () => {
    vi.stubEnv('MODE', 'demo')
    try {
      const LoginPage = Route.component!
      render(<LoginPage />)

      expect(screen.queryByPlaceholderText('Paste your token here...')).not.toBeInTheDocument()
      expect(screen.queryByText(/kubectl create token/)).not.toBeInTheDocument()
      fireEvent.click(screen.getByRole('button', { name: 'Sign in to the demo' }))

      expect(useAuthStore.getState().token).toBe(DEMO_TOKEN)
      await waitFor(() => expect(mockNavigate).toHaveBeenCalledWith({ href: '/' }))
    } finally {
      vi.unstubAllEnvs()
    }
  })
})
