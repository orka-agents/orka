import { act, renderHook, waitFor } from '@testing-library/react'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { useCallerNamespace } from './use-caller-namespace'

function whoami(namespace?: string) {
  let calls = 0
  server.use(http.get('/api/v1/auth/whoami', () => {
    calls += 1
    return HttpResponse.json({ authenticated: true, authType: 'oidc', username: 'alice', ...(namespace && { namespace }) })
  }))
  return () => calls
}

beforeEach(() => {
  useAuthStore.setState({ token: 'oidc-token' })
  useUIStore.setState({ namespace: 'orka-system', recentNamespaces: [], callerNamespace: null })
})

describe('useCallerNamespace', () => {
  it('lands an OIDC user bound to team-a in team-a', async () => {
    whoami('team-a')
    renderHook(() => useCallerNamespace())
    await waitFor(() => expect(useUIStore.getState().namespace).toBe('team-a'))
    expect(useUIStore.getState().callerNamespace).toBe('team-a')
    expect(useUIStore.getState().recentNamespaces).toEqual(['team-a'])
  })

  it('keeps a namespace the same caller chose later', async () => {
    const calls = whoami('team-a')
    useUIStore.setState({ namespace: 'team-b', recentNamespaces: ['team-b', 'team-a'], callerNamespace: 'team-a' })
    renderHook(() => useCallerNamespace())
    await waitFor(() => expect(calls()).toBe(1))
    expect(useUIStore.getState().namespace).toBe('team-b')
  })

  it('leaves the namespace alone when the caller has none', async () => {
    const calls = whoami()
    useUIStore.setState({ callerNamespace: 'team-a' })
    renderHook(() => useCallerNamespace())
    await waitFor(() => expect(useUIStore.getState().callerNamespace).toBeNull())
    expect(calls()).toBe(1)
    expect(useUIStore.getState().namespace).toBe('orka-system')
  })

  it('does not ask without a token', () => {
    const calls = whoami('team-a')
    useAuthStore.setState({ token: null })
    renderHook(() => useCallerNamespace())
    expect(calls()).toBe(0)
    expect(useUIStore.getState().namespace).toBe('orka-system')
  })

  it('ignores an answer for a token that was replaced', async () => {
    const pending: { release?: () => void } = {}
    server.use(http.get('/api/v1/auth/whoami', () => new Promise<Response>((resolve) => {
      pending.release = () => resolve(HttpResponse.json({ namespace: 'team-a' }))
    })))
    renderHook(() => useCallerNamespace())
    await waitFor(() => expect(pending.release).toBeDefined())
    act(() => useAuthStore.setState({ token: null }))
    pending.release?.()
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)))
    expect(useUIStore.getState().namespace).toBe('orka-system')
  })
})
