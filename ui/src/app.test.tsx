import { describe, it, expect, vi } from 'vitest'
import { act, render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createRouter: () => ({}),
    RouterProvider: () => <div data-testid="router">Router</div>,
  }
})

vi.mock('./routeTree.gen', () => ({
  routeTree: {},
}))

import { App } from './app'
import { useAuthStore } from './stores/auth'
import { useChatStore } from './stores/chat'
import { useOversightAlerts } from './stores/oversight-alerts'
import { useUIStore } from './stores/ui'

describe('App', () => {
  it('renders without crashing', () => {
    render(<App />)
    expect(screen.getByTestId('router')).toBeInTheDocument()
  })

  it('resets chat when the namespace changes outside the Chat page', () => {
    act(() => useUIStore.getState().setNamespace('default'))
    useChatStore.setState({
      activeNamespace: 'default',
      messages: [{ id: 'message-1', role: 'user', content: 'still running', timestamp: '2026-08-31T00:00:00Z' }],
      currentSessionId: 'session-default',
      isStreaming: true,
      turnEpoch: 4,
    })

    act(() => useUIStore.getState().setNamespace('team'))

    expect(useChatStore.getState()).toMatchObject({
      activeNamespace: 'team',
      messages: [],
      currentSessionId: null,
      isStreaming: false,
      turnEpoch: 5,
    })
  })

  it('drops oversight alerts when the identity changes', () => {
    act(() => useAuthStore.setState({ token: 'first-user' }))
    useOversightAlerts.getState().add({
      decisionID: 't@1', namespace: 'team', taskName: 't', observedAt: '2026-08-31T00:00:00Z', rule: 'Rule 1', sentence: 'kept to the first user',
    })
    act(() => useAuthStore.setState({ token: 'second-user' }))
    expect(useOversightAlerts.getState().alerts).toEqual([])
  })
})
