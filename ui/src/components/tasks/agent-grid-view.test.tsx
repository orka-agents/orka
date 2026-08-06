import { describe, it, expect, beforeEach, vi } from 'vitest'
import {
  act,
  createTestQueryClient,
  render,
  screen,
  waitFor,
} from '@/test/test-utils'
import { render as rawRender } from '@testing-library/react'
import { QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/live' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { AgentGridView } from './agent-grid-view'

describe('AgentGridView', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows loading skeletons', () => {
    server.use(http.get('/api/v1/tasks', async () => {
      await new Promise(r => setTimeout(r, 5000))
      return HttpResponse.json({ items: [], metadata: {} })
    }))
    const { container } = render(<AgentGridView />)
    expect(container.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0)
  })

  it('shows empty state when no running tasks', async () => {
    render(<AgentGridView />)
    await waitFor(() => {
      expect(screen.getByText(/No tasks currently running/)).toBeInTheDocument()
    })
  })

  it('shows running tasks as cards', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [
        { metadata: { name: 'running-1', namespace: 'default', uid: 'u1' }, spec: { type: 'agent', agentRef: { name: 'my-agent' } }, status: { phase: 'Running', startTime: new Date().toISOString() } },
        { metadata: { name: 'succeeded-1', namespace: 'default', uid: 'u2' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } },
      ],
      metadata: {},
    })))
    render(<AgentGridView />)
    await waitFor(() => {
      expect(screen.getByText('running-1')).toBeInTheDocument()
    })
    expect(screen.queryByText('succeeded-1')).not.toBeInTheDocument()
    expect(screen.getByText('my-agent')).toBeInTheDocument()
  })

  it('shows correct count', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [
        { metadata: { name: 'r1', namespace: 'default', uid: 'u1' }, spec: { type: 'agent' }, status: { phase: 'Running', startTime: new Date().toISOString() } },
        { metadata: { name: 'r2', namespace: 'default', uid: 'u2' }, spec: { type: 'ai' }, status: { phase: 'Running', startTime: new Date().toISOString() } },
      ],
      metadata: {},
    })))
    render(<AgentGridView />)
    await waitFor(() => {
      expect(screen.getByText('2 active tasks')).toBeInTheDocument()
    })
  })

  it('loads every task page before reporting active agents', async () => {
    const requests: Array<string | null> = []
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        requests.push(cursor)
        if (!cursor) {
          return HttpResponse.json({
            items: [],
            metadata: { continue: 'page-2' },
          })
        }
        return HttpResponse.json({
          items: [
            {
              metadata: {
                name: 'second-page-agent',
                namespace: 'default',
                uid: 'second-page-agent',
              },
              spec: { type: 'agent' },
              status: { phase: 'Running', startTime: new Date().toISOString() },
            },
          ],
          metadata: {},
        })
      }),
    )

    render(<AgentGridView />)

    expect(await screen.findByText('second-page-agent')).toBeInTheDocument()
    expect(screen.getByText('1 active task')).toBeInTheDocument()
    expect(requests).toEqual([null, 'page-2'])
  })

  it('keeps active-agent counts loading until the final task page arrives', async () => {
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

    render(<AgentGridView />)

    await waitFor(() => expect(requests).toBe(2))
    expect(
      screen.getByRole('status', {
        name: /loading complete task inventory/i,
      }),
    ).toBeInTheDocument()
    expect(screen.queryByText('0 active tasks')).not.toBeInTheDocument()

    releaseFinalPage()

    expect(await screen.findByText('0 active tasks')).toBeInTheDocument()
  })

  it('does not show an empty inventory when complete pagination fails', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [],
          metadata: { continue: 'same-cursor' },
        }),
      ),
    )

    render(<AgentGridView />)

    expect(
      await screen.findByRole('alert', {
        name: /unable to load complete task inventory/i,
      }),
    ).toBeInTheDocument()
    expect(screen.getByText('Task inventory unavailable')).toBeInTheDocument()
    expect(screen.queryByText('0 active tasks')).not.toBeInTheDocument()
    expect(
      screen.queryByText(/No tasks currently running/i),
    ).not.toBeInTheDocument()
  })

  it('hides cached counts when a complete-inventory refetch fails', async () => {
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
    const queryClient = createTestQueryClient()

    rawRender(
      <QueryClientProvider client={queryClient}>
        <AgentGridView />
      </QueryClientProvider>,
    )

    expect(await screen.findByText('0 active tasks')).toBeInTheDocument()
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
    expect(screen.getByText('Task inventory unavailable')).toBeInTheDocument()
    expect(screen.queryByText('0 active tasks')).not.toBeInTheDocument()
    expect(
      screen.queryByText(/No tasks currently running/i),
    ).not.toBeInTheDocument()
    expect(calls).toBe(3)
  })
})
