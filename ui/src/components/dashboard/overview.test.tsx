import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/' }),
    Outlet: () => <div data-testid="outlet" />,
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Overview } from './overview'

describe('Overview', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('renders Dashboard heading', () => {
    render(<Overview />)
    expect(screen.getByText('Dashboard')).toBeInTheDocument()
  })

  it('renders without crashing', () => {
    render(<Overview />)
    expect(screen.getByText('Overview of your Orka workspace')).toBeInTheDocument()
  })

  it('includes Scheduled and Cancelled tasks in the phase distribution', async () => {
    const mk = (name: string, phase: string) => ({
      metadata: { name, namespace: 'default', uid: name, creationTimestamp: new Date().toISOString() },
      spec: { type: 'container' },
      status: { phase },
    })
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [mk('a', 'Running'), mk('b', 'Scheduled'), mk('c', 'Cancelled')],
          metadata: {},
        }),
      ),
    )
    render(<Overview />)
    const heading = await screen.findByText('Phase Distribution')
    // Scope assertions to the distribution card (the phase labels also appear
    // as StatusDots in the Recent Tasks list).
    const card = heading.closest('[data-slot="card"]') as HTMLElement
    expect(card).not.toBeNull()
    await waitFor(() => {
      expect(within(card).getByText('Scheduled')).toBeInTheDocument()
    })
    expect(within(card).getByText('Cancelled')).toBeInTheDocument()
    expect(within(card).getByText('Running')).toBeInTheDocument()
  })
})
