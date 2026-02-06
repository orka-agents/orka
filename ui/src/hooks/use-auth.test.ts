import { renderHook } from '@testing-library/react'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useAuthStore } from '@/stores/auth'

const mockNavigate = vi.fn()
const mockLocation = { pathname: '/dashboard' }

vi.mock('@tanstack/react-router', () => ({
  useNavigate: () => mockNavigate,
  useLocation: () => mockLocation,
}))

import { useAuthGuard } from './use-auth'

beforeEach(() => {
  mockNavigate.mockClear()
  mockLocation.pathname = '/dashboard'
  useAuthStore.setState({ token: null })
})

describe('useAuthGuard', () => {
  it('returns isAuthenticated=false and navigates to /login when no token', () => {
    const { result } = renderHook(() => useAuthGuard())
    expect(result.current.isAuthenticated).toBe(false)
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/login' })
  })

  it('returns isAuthenticated=true and does not navigate when token is set', () => {
    useAuthStore.setState({ token: 'test-token' })
    const { result } = renderHook(() => useAuthGuard())
    expect(result.current.isAuthenticated).toBe(true)
    expect(mockNavigate).not.toHaveBeenCalled()
  })

  it('does not navigate when on /login path even without token', () => {
    mockLocation.pathname = '/login'
    const { result } = renderHook(() => useAuthGuard())
    expect(result.current.isAuthenticated).toBe(false)
    expect(mockNavigate).not.toHaveBeenCalled()
  })
})
