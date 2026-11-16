import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { render as rawRender } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
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
    useLocation: () => ({ pathname: '/kanban' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { KanbanBoard } from './kanban-board'

describe('KanbanBoard', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('loading state shows skeletons', () => {
    server.use(
      http.get('/api/v1/tasks', async () => {
        await new Promise((r) => setTimeout(r, 5000))
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )
    const { container } = render(<KanbanBoard />)
    const skeletons = container.querySelectorAll('[data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('empty state shows "No ... tasks" messages', async () => {
    render(<KanbanBoard />)
    await waitFor(() => {
      expect(screen.getByText('No pending tasks')).toBeInTheDocument()
    })
    expect(screen.getByText('No running tasks')).toBeInTheDocument()
    expect(screen.getByText('No succeeded tasks')).toBeInTheDocument()
    expect(screen.getByText('No failed tasks')).toBeInTheDocument()
    expect(screen.getByText('No scheduled tasks')).toBeInTheDocument()
    expect(screen.getByText('No cancelled tasks')).toBeInTheDocument()
  })

  it('has a column for every task phase and buckets Scheduled/Cancelled in their own columns', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'sched-task', namespace: 'default', uid: 's1', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Scheduled' },
            },
            {
              metadata: { name: 'cancel-task', namespace: 'default', uid: 'c1', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Cancelled' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<KanbanBoard />)
    await waitFor(() => expect(screen.getByText('sched-task')).toBeInTheDocument())
    expect(screen.getByText('cancel-task')).toBeInTheDocument()
    // Column headers exist for the new phases.
    expect(screen.getByText('Scheduled')).toBeInTheDocument()
    expect(screen.getByText('Cancelled')).toBeInTheDocument()
    // The Pending column stays empty — Scheduled/Cancelled are NOT mis-bucketed.
    expect(screen.getByText('No pending tasks')).toBeInTheDocument()
  })

  it('populated board shows tasks in correct columns', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'pending-task', namespace: 'default', uid: 'u1', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Pending' },
            },
            {
              metadata: { name: 'running-task', namespace: 'default', uid: 'u2', creationTimestamp: new Date().toISOString() },
              spec: { type: 'ai' },
              status: { phase: 'Running', startTime: new Date(Date.now() - 120_000).toISOString() },
            },
            {
              metadata: { name: 'done-task', namespace: 'prod', uid: 'u3', creationTimestamp: new Date().toISOString() },
              spec: { type: 'agent', agentRef: { name: 'my-agent' } },
              status: { phase: 'Succeeded' },
            },
            {
              metadata: { name: 'fail-task', namespace: 'default', uid: 'u4', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Failed' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<KanbanBoard />)
    await waitFor(() => {
      expect(screen.getByText('pending-task')).toBeInTheDocument()
    })
    expect(screen.getByText('running-task')).toBeInTheDocument()
    expect(screen.getByText('done-task')).toBeInTheDocument()
    expect(screen.getByText('fail-task')).toBeInTheDocument()
  })

  it('column headers show correct titles', async () => {
    render(<KanbanBoard />)
    await waitFor(() => {
      expect(screen.getByText('Pending')).toBeInTheDocument()
    })
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('Failed')).toBeInTheDocument()
  })

  it('page title is Board', async () => {
    render(<KanbanBoard />)
    expect(screen.getByText('Board')).toBeInTheDocument()
  })

  it('each column has a phase-keyed top rail from the token system', async () => {
    render(<KanbanBoard />)
    await waitFor(() => {
      expect(screen.getByText('Pending')).toBeInTheDocument()
    })
    const rails: Record<string, string> = {
      Pending: 'border-status-pending',
      Running: 'border-status-running',
      Succeeded: 'border-status-succeeded',
      Failed: 'border-status-failed',
    }
    for (const [label, railClass] of Object.entries(rails)) {
      const column = screen.getByText(label).closest('.border-t-2')
      expect(column).not.toBeNull()
      expect(column).toHaveClass(railClass)
    }
  })

  it('count chips use the status text token on the card surface, not pastels', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'r1', namespace: 'default', uid: 'r1', creationTimestamp: new Date().toISOString() },
              spec: { type: 'ai' },
              status: { phase: 'Running' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<KanbanBoard />)
    const runningHeader = await screen.findByText('Running')
    const header = runningHeader.parentElement!
    const chip = header.querySelector('span:last-child')!
    await waitFor(() => {
      expect(chip).toHaveTextContent('1')
    })
    expect(chip).toHaveClass('text-status-running')
    // Not the column's own tint: tint on tint fails AA contrast.
    expect(chip).toHaveClass('bg-card')
    expect(chip).not.toHaveClass('bg-status-running-bg')
    expect(chip.className).not.toMatch(/bg-(yellow|blue|green|red)-/)
  })

  it('shows a live pulse indicator ONLY on the Running column (liveness scarcity)', async () => {
    render(<KanbanBoard />)
    await waitFor(() => {
      expect(screen.getByText('Running')).toBeInTheDocument()
    })
    // Exactly one live indicator, and it lives in the Running column header.
    const indicators = screen.getAllByTestId('live-indicator')
    expect(indicators).toHaveLength(1)
    const indicator = indicators[0]
    expect(indicator.className).toContain('bg-live')
    expect(indicator.className).toContain('animate-pulse-live')
    // It sits beside the "Running" heading, not Pending/Succeeded/Failed.
    expect(indicator.parentElement?.textContent).toContain('Running')
    for (const terminal of ['Pending', 'Succeeded', 'Failed']) {
      const header = screen.getByText(terminal).parentElement!
      expect(header.querySelector('[data-testid="live-indicator"]')).toBeNull()
    }
  })

  it('follows continue tokens so tasks past the first page are counted', async () => {
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const url = new URL(request.url)
        if (url.searchParams.get('continue') === 'page-2') {
          return HttpResponse.json({
            items: [{ metadata: { name: 'second-page-task', namespace: 'default', uid: 'p2' }, spec: { type: 'ai' }, status: { phase: 'Running' } }],
            metadata: {},
          })
        }
        return HttpResponse.json({
          items: [{ metadata: { name: 'first-page-task', namespace: 'default', uid: 'p1' }, spec: { type: 'ai' }, status: { phase: 'Running' } }],
          metadata: { continue: 'page-2' },
        })
      }),
    )
    render(<KanbanBoard />)
    expect(await screen.findByText('second-page-task')).toBeInTheDocument()
    expect(screen.getByText('first-page-task')).toBeInTheDocument()
    const chip = screen.getByText('Running').parentElement!.querySelector('span:last-child')!
    expect(chip).toHaveTextContent('2')
    expect(screen.queryByText(/More tasks exist/)).not.toBeInTheDocument()
  })

  it('shows a not-authorized panel instead of empty columns on 403', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      ),
    )
    render(<KanbanBoard />)
    expect(await screen.findByText('Not authorized to view tasks')).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('not authorized')
    expect(screen.queryByText('No pending tasks')).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Pending' })).not.toBeInTheDocument()
  })

  it('shows the server error instead of empty columns when the walk fails', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({ error: { code: 500, message: 'etcd unavailable' } }, { status: 500 }),
      ),
    )
    // The hook retries server errors; skip the backoff so the error settles.
    const client = new QueryClient({ defaultOptions: { queries: { retryDelay: 0, gcTime: 0 } } })
    rawRender(<QueryClientProvider client={client}><KanbanBoard /></QueryClientProvider>)
    expect(await screen.findByText('Could not load tasks')).toBeInTheDocument()
    expect(screen.getByText('etcd unavailable')).toBeInTheDocument()
    expect(screen.queryByText('No pending tasks')).not.toBeInTheDocument()
  })

  it('says the counts are partial when the bounded walk stops early', async () => {
    let page = 0
    server.use(
      http.get('/api/v1/tasks', () => {
        page += 1
        return HttpResponse.json({
          items: [{ metadata: { name: `task-${page}`, namespace: 'default', uid: `u-${page}` }, spec: { type: 'ai' }, status: { phase: 'Succeeded' } }],
          metadata: { continue: `page-${page + 1}` },
        })
      }),
    )
    render(<KanbanBoard />)
    expect(await screen.findByText('Column counts use 20 loaded tasks in resource-key order. More tasks exist.')).toBeInTheDocument()
  })

  it('attention view groups Tasks into Needs you, Agent working and Done', async () => {
    const now = new Date().toISOString()
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            { metadata: { name: 'needs-approval', namespace: 'default', uid: 'u-approval', creationTimestamp: now }, spec: { type: 'agent' }, status: { phase: 'Running' } },
            { metadata: { name: 'busy', namespace: 'default', uid: 'u-busy', creationTimestamp: now }, spec: { type: 'agent' }, status: { phase: 'Running' } },
            { metadata: { name: 'capacity', namespace: 'default', uid: 'u-cap', creationTimestamp: now }, spec: { type: 'agent' }, status: { phase: 'Pending', conditions: [{ type: 'WaitingForCapacity', status: 'True' }] } },
            { metadata: { name: 'timer', namespace: 'default', uid: 'u-timer', creationTimestamp: now }, spec: { type: 'agent' }, status: { phase: 'Succeeded', wait: { kind: 'Timer', state: 'Waiting' } } },
            { metadata: { name: 'finished', namespace: 'default', uid: 'u-done', creationTimestamp: now }, spec: { type: 'agent' }, status: { phase: 'Succeeded' } },
          ],
          metadata: {},
        }),
      ),
      http.get('/api/v1/attention', () =>
        HttpResponse.json({
          namespace: 'default', generatedAt: now, total: 1, counts: { approval: 1 }, incomplete: false,
          sources: { approvals: 'included', monitors: 'included' }, metadata: {},
          items: [{
            id: 'approval:u-approval:ap', kind: 'approval', rank: 1, since: now, summary: 'Approval requested for github',
            task: { namespace: 'default', name: 'needs-approval', uid: 'u-approval', phase: 'Running' },
            approval: { id: 'ap', source: 'brokered', targetTool: 'github', canApprove: true, canDecline: true },
          }],
        }),
      ),
    )
    render(<KanbanBoard view="attention" />)
    const needsYou = await screen.findByRole('region', { name: 'Needs you' })
    await waitFor(() => expect(needsYou).toHaveTextContent('Approval requested for github'))
    const working = screen.getByRole('region', { name: 'Agent working' })
    const done = screen.getByRole('region', { name: 'Done' })
    await waitFor(() => expect(working).toHaveTextContent('busy'))
    expect(working).toHaveTextContent('capacity')
    expect(working).toHaveTextContent('timer')
    expect(working).not.toHaveTextContent('needs-approval')
    expect(done).toHaveTextContent('finished')
    expect(done).not.toHaveTextContent('needs-approval')
  })

  it('attention view says when the ranking is incomplete', async () => {
    server.use(
      http.get('/api/v1/attention', () =>
        HttpResponse.json({
          namespace: 'default', generatedAt: new Date().toISOString(), total: 0, counts: {}, incomplete: true,
          incompleteReasons: ['approvalCandidateLimit'], sources: { approvals: 'included', monitors: 'forbidden' }, metadata: {}, items: [],
        }),
      ),
    )
    render(<KanbanBoard view="attention" />)
    expect(await screen.findByText(/may be incomplete: too many Tasks were waiting for approval/)).toBeInTheDocument()
  })

  it('offers both board views', async () => {
    render(<KanbanBoard view="phase" />)
    const nav = screen.getByRole('navigation', { name: 'Board view' })
    expect(nav).toHaveTextContent('Needs you')
    expect(nav).toHaveTextContent('By phase')
  })

  it('attention view walks later attention pages before placing Tasks in Done', async () => {
    const now = new Date().toISOString()
    const failed = (name: string) => ({
      id: `failed:u-${name}`, kind: 'failed', rank: 3, since: now, summary: 'Task failed',
      task: { namespace: 'default', name, uid: `u-${name}`, phase: 'Failed' },
    })
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: ['failed-a', 'failed-b'].map((name) => ({ metadata: { name, namespace: 'default', uid: `u-${name}`, creationTimestamp: now }, spec: { type: 'agent' }, status: { phase: 'Failed' } })),
          metadata: {},
        }),
      ),
      http.get('/api/v1/attention', ({ request }) => {
        const second = new URL(request.url).searchParams.get('continue') === 'next'
        return HttpResponse.json({
          namespace: 'default', generatedAt: now, total: 2, counts: { failed: 2 }, incomplete: false,
          sources: { approvals: 'included', monitors: 'included' },
          metadata: second ? {} : { continue: 'next' }, items: [failed(second ? 'failed-b' : 'failed-a')],
        })
      }),
    )
    render(<KanbanBoard view="attention" />)
    const needsYou = await screen.findByRole('region', { name: 'Needs you' })
    await waitFor(() => expect(needsYou).toHaveTextContent('failed-b'))
    expect(needsYou).toHaveTextContent('failed-a')
    expect(screen.getByRole('region', { name: 'Done' })).not.toHaveTextContent('failed-b')
  })

  it('attention view flags Tasks it could not classify past the walk bound', async () => {
    const now = new Date().toISOString()
    let page = 0
    server.use(
      http.get('/api/v1/attention', () => {
        page += 1
        return HttpResponse.json({
          namespace: 'default', generatedAt: now, total: 5000, counts: { failed: 5000 }, incomplete: false,
          sources: { approvals: 'included', monitors: 'included' }, metadata: { continue: `page-${page}` },
          items: [{ id: `failed:${page}`, kind: 'failed', rank: 3, since: now, summary: 'Task failed', task: { namespace: 'default', name: `t-${page}`, uid: `u-${page}` } }],
        })
      }),
    )
    render(<KanbanBoard view="attention" />)
    expect(await screen.findByText(/Agent working and Done may include Tasks that need you/)).toBeInTheDocument()
    expect(page).toBe(10)
  })
})
