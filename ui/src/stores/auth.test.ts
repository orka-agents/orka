import { describe, it, expect, beforeEach, vi } from 'vitest'

// Make persist a pass-through so stores work without localStorage
vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useAuthStore } from './auth'

describe('useAuthStore', () => {
  beforeEach(() => {
    useAuthStore.setState({ token: null })
  })

  it('has null token initially', () => {
    expect(useAuthStore.getState().token).toBeNull()
  })

  it('setToken updates token', () => {
    useAuthStore.getState().setToken('abc123')
    expect(useAuthStore.getState().token).toBe('abc123')
  })

  it('clearToken sets token to null', () => {
    useAuthStore.getState().setToken('abc123')
    useAuthStore.getState().clearToken()
    expect(useAuthStore.getState().token).toBeNull()
  })

  it('isAuthenticated returns true when token is set', () => {
    useAuthStore.getState().setToken('abc123')
    expect(useAuthStore.getState().isAuthenticated()).toBe(true)
  })

  it('isAuthenticated returns false when token is null', () => {
    expect(useAuthStore.getState().isAuthenticated()).toBe(false)
  })
})
