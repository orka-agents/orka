import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import userEvent from '@testing-library/user-event'
import { render as rawRender } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to }: any) => <a href={to}>{children}</a> }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { RuntimeCanvas } from './runtime-canvas'

const running = (name: string, extra: Record<string, unknown> = {}) => ({
  metadata: { name, namespace: 'default', uid: name },
  spec: { type: 'agent', agentRef: { name: 'alpha' } },
  status: { phase: 'Running', startTime: new Date().toISOString(), ...extra },
})

describe('RuntimeCanvas', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows loading skeletons', () => {
    server.use(http.get('/api/v1/tasks', async () => {
      await new Promise((r) => setTimeout(r, 5000))
      return HttpResponse.json({ items: [], metadata: {} })
    }))
    const { container } = render(<RuntimeCanvas />)
    expect(container.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0)
  })

  it('shows namespace-scoped empty state', async () => {
    render(<RuntimeCanvas />)
    await waitFor(() => {
      expect(screen.getByText(/No tasks in namespace "default"/)).toBeInTheDocument()
    })
    expect(screen.getByText(/only the selected namespace/)).toBeInTheDocument()
  })

  it('renders spotlight + roster for running tasks', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [running('r1'), running('r2')], metadata: {},
    })))
    render(<RuntimeCanvas />)
    await waitFor(() => expect(screen.getByText('2 active · namespace default')).toBeInTheDocument())
    expect(screen.getByText('Active now')).toBeInTheDocument()
    expect(screen.getByText('Agents')).toBeInTheDocument()
  })

  it('selects a stable active task when none has startTime', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [
        { metadata: { name: 'b', namespace: 'default', uid: 'b' }, spec: { type: 'agent' }, status: { phase: 'Running' } },
        { metadata: { name: 'a', namespace: 'default', uid: 'a' }, spec: { type: 'agent' }, status: { phase: 'Running' } },
      ],
      metadata: {},
    })))
    render(<RuntimeCanvas />)
    // ascending name fallback => 'a' spotlit; render must not crash on missing startTime
    await waitFor(() => expect(screen.getAllByText('a').length).toBeGreaterThan(0))
  })

  it('does not fetch non-spotlight running task event streams while computing activity', async () => {
    let selectedCalls = 0
    let backgroundCalls = 0
    server.use(
      http.get('/api/v1/tasks', () => HttpResponse.json({
        items: [
          running('selected', { startTime: '2026-06-28T11:00:00Z' }),
          running('background', { startTime: '2026-06-28T10:00:00Z' }),
        ],
        metadata: {},
      })),
      http.get('/api/v1/tasks/selected/events', () => {
        selectedCalls += 1
        return HttpResponse.json({ namespace: 'default', streamType: 'task', streamID: 'selected', afterSeq: 0, latestSeq: 0, events: [] })
      }),
      http.get('/api/v1/tasks/background/events', () => {
        backgroundCalls += 1
        return HttpResponse.json({ error: 'events disabled' }, { status: 501 })
      }),
    )

    render(<RuntimeCanvas />)

    await waitFor(() => expect(selectedCalls).toBeGreaterThanOrEqual(1))
    await new Promise((resolve) => setTimeout(resolve, 80))
    expect(backgroundCalls).toBe(0)
  })

  it('selects the active task from cached latest event activity, not only start time', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } },
    })
    queryClient.setQueryData(['taskEvents', 'older-active', 'default', 'older-active'], {
      namespace: 'default', streamType: 'task', streamID: 'older-active', afterSeq: 0, latestSeq: 9,
      events: [{ id: 'e9', namespace: 'default', streamType: 'task', streamID: 'older-active', seq: 9, type: 'ToolCallCompleted', severity: 'info', summary: 'older task just ran a tool', createdAt: '2026-06-28T12:00:00Z' }],
    })
    queryClient.setQueryData(['taskEvents', 'newer-idle', 'default', 'newer-idle'], {
      namespace: 'default', streamType: 'task', streamID: 'newer-idle', afterSeq: 0, latestSeq: 1,
      events: [{ id: 'e1', namespace: 'default', streamType: 'task', streamID: 'newer-idle', seq: 1, type: 'TaskStarted', severity: 'info', summary: 'newer task started', createdAt: '2026-06-28T11:05:00Z' }],
    })
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [
        running('older-active', { startTime: '2026-06-28T10:00:00Z' }),
        running('newer-idle', { startTime: '2026-06-28T11:00:00Z' }),
      ],
      metadata: {},
    })))

    rawRender(
      <QueryClientProvider client={queryClient}>
        <RuntimeCanvas />
      </QueryClientProvider>,
    )

    await waitFor(() => expect(screen.getByText('older task just ran a tool')).toBeInTheDocument())
  })

  it('surfaces the active task latest event summary in the spotlight', async () => {
    server.use(
      http.get('/api/v1/tasks', () => HttpResponse.json({ items: [running('r1')], metadata: {} })),
      http.get('/api/v1/tasks/r1/events', () => HttpResponse.json({
        namespace: 'default', streamType: 'task', streamID: 'r1', afterSeq: 0, latestSeq: 2,
        events: [{ id: 'e2', namespace: 'default', streamType: 'task', streamID: 'r1', seq: 2, type: 'ToolCallCompleted', severity: 'info', summary: 'ran web_search', createdAt: new Date().toISOString() }],
      })),
    )
    render(<RuntimeCanvas />)
    await waitFor(() => expect(screen.getByText('ran web_search')).toBeInTheDocument())
  })

  it('refreshes the active task spotlight event while following is paused', async () => {
    const user = userEvent.setup()
    let eventCalls = 0
    server.use(
      http.get('/api/v1/tasks', () => HttpResponse.json({ items: [running('r1')], metadata: {} })),
      http.get('/api/v1/tasks/r1/events', () => {
        const summary = eventCalls++ === 0 ? 'old tool output' : 'fresh tool output'
        return HttpResponse.json({
          namespace: 'default', streamType: 'task', streamID: 'r1', afterSeq: 0, latestSeq: eventCalls,
          events: [{ id: `e-${eventCalls}`, namespace: 'default', streamType: 'task', streamID: 'r1', seq: eventCalls, type: 'ToolCallCompleted', severity: 'info', summary, createdAt: new Date().toISOString() }],
        })
      }),
    )

    render(<RuntimeCanvas />)

    await waitFor(() => expect(screen.getByText('old tool output')).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /following/i }))
    await user.click(screen.getByRole('button', { name: /refresh/i }))

    await waitFor(() => expect(screen.getByText('fresh tool output')).toBeInTheDocument())
  })

  it('shows empty state with only terminal tasks (header 0 active, no roster agents)', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({
      items: [{ metadata: { name: 's1', namespace: 'default', uid: 's1' }, spec: { type: 'agent', agentRef: { name: 'alpha' } }, status: { phase: 'Succeeded' } }],
      metadata: {},
    })))
    render(<RuntimeCanvas />)
    await waitFor(() => expect(screen.getByText('0 active · namespace default')).toBeInTheDocument())
    // roster gets running-only, so the header count and roster stay consistent
    await waitFor(() => expect(screen.getByText('No agents active')).toBeInTheDocument())
    expect(screen.getByText('No active task')).toBeInTheDocument()
  })

  it('exposes no simulator controls in production canvas', async () => {
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({ items: [running('r1')], metadata: {} })))
    render(<RuntimeCanvas />)
    await waitFor(() => expect(screen.getByText('Active now')).toBeInTheDocument())
    expect(screen.queryByText('SIMULATOR')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /inject failure/i })).not.toBeInTheDocument()
  })
})
