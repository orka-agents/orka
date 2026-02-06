import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, fireEvent } from '@/test/test-utils'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

const mockNavigate = vi.fn()

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createFileRoute: (_path: string) => (opts: any) => ({ ...opts, path: _path }),
    useNavigate: () => mockNavigate,
    useLocation: () => ({ pathname: '/login' }),
  }
})

import { useAuthStore } from '@/stores/auth'
import { Route } from './login'

describe('login route', () => {
  beforeEach(() => {
    mockNavigate.mockClear()
    useAuthStore.setState({ token: null })
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
    expect(screen.getByText('Mercan')).toBeInTheDocument()
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

  it('submitting sets token in auth store', () => {
    const LoginPage = Route.component!
    render(<LoginPage />)
    const input = screen.getByPlaceholderText('Paste your token here...')
    fireEvent.change(input, { target: { value: 'my-secret-token' } })
    fireEvent.click(screen.getByRole('button', { name: /sign in/i }))
    expect(useAuthStore.getState().token).toBe('my-secret-token')
  })

  it('navigates to / when already authenticated', () => {
    useAuthStore.setState({ token: 'existing-token' })
    const LoginPage = Route.component!
    render(<LoginPage />)
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/' })
  })

  it('handles #token=... hash fragment from CLI login', () => {
    const originalHash = window.location.hash
    window.location.hash = '#token=hash-cli-token'
    const LoginPage = Route.component!
    render(<LoginPage />)
    expect(useAuthStore.getState().token).toBe('hash-cli-token')
    window.location.hash = originalHash
  })
})
