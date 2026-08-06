import { describe, it, expect, beforeEach, vi } from 'vitest'
import {
  act,
  render,
  screen,
  waitFor,
  within,
} from '@/test/test-utils'
import { render as rawRender } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
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

  it('loads every task page before reporting dashboard inventory', async () => {
    const requests: Array<string | null> = []
    const mk = (name: string) => ({
      metadata: { name, namespace: 'default', uid: name },
      spec: { type: 'container' },
      status: { phase: 'Running' },
    })
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        requests.push(cursor)
        if (!cursor) {
          return HttpResponse.json({
            items: [mk('first-page-task')],
            metadata: { continue: 'page-2' },
          })
        }
        return HttpResponse.json({
          items: [mk('second-page-task')],
          metadata: {},
        })
      }),
    )

    render(<Overview />)

    expect(await screen.findByText('second-page-task')).toBeInTheDocument()
    expect(screen.getByText('first-page-task')).toBeInTheDocument()
    expect(requests).toEqual([null, 'page-2'])
  })

  it('keeps task-derived dashboard views loading until the final page arrives', async () => {
    let releaseFinalPage: () => void = () => {}
    const finalPageGate = new Promise<void>((resolve) => {
      releaseFinalPage = resolve
    })
    let requests = 0
    server.use(
      http.get('/api/v1/tasks', async ({ request }) => {
        requests += 1
        const cursor = new URL(request.url).searchParams.get('continue')
        if (!cursor) {
          return HttpResponse.json({
            items: [],
            metadata: { continue: 'page-2' },
          })
        }
        await finalPageGate
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )

    render(<Overview />)

    await waitFor(() => expect(requests).toBe(2))
    expect(
      screen.getByRole('status', {
        name: /loading complete task inventory/i,
      }),
    ).toBeInTheDocument()
    expect(screen.queryByText('Phase Distribution')).not.toBeInTheDocument()

    releaseFinalPage()

    expect(await screen.findByText('Phase Distribution')).toBeInTheDocument()
  })

  it('does not report task counts when complete inventory pagination fails', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [],
          metadata: { continue: 'same-cursor' },
        }),
      ),
    )

    render(<Overview />)

    expect(
      await screen.findByRole('alert', {
        name: /unable to load complete task inventory/i,
      }),
    ).toBeInTheDocument()
    expect(screen.queryByText('Phase Distribution')).not.toBeInTheDocument()
  })

  it('hides cached task counts when a complete-inventory refetch fails', async () => {
    let calls = 0
    server.use(
      http.get('/api/v1/tasks', () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.json({ items: [], metadata: {} })
        }
        return HttpResponse.json({
          items: [],
          metadata: { continue: 'same-cursor' },
        })
      }),
    )
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: 1, retryDelay: 0, gcTime: 0 },
        mutations: { retry: false },
      },
    })

    rawRender(
      <QueryClientProvider client={queryClient}>
        <Overview />
      </QueryClientProvider>,
    )

    expect(await screen.findByText('Phase Distribution')).toBeInTheDocument()
    await act(async () => {
      await queryClient.refetchQueries({
        queryKey: ['tasks', 'all', 'default', '100'],
      })
    })

    expect(
      await screen.findByRole('alert', {
        name: /unable to load complete task inventory/i,
      }),
    ).toBeInTheDocument()
    expect(screen.queryByText('Phase Distribution')).not.toBeInTheDocument()
    expect(calls).toBe(3)
  })
})
