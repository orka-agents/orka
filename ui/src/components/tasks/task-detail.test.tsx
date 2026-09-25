import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockNavigate = vi.fn()
const mockSearch: { current: { tab?: string } } = { current: { tab: 'overview' } }
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => mockNavigate,
    useLocation: () => ({ pathname: '/tasks/test-task' }),
    useSearch: () => mockSearch.current,
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskDetail } from './task-detail'
import { render as renderRaw } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

describe('TaskDetail', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    mockNavigate.mockClear()
    mockSearch.current = { tab: 'overview' }
  })

  it('loading state shows skeletons', () => {
    server.use(
      http.get('/api/v1/tasks/:id', async () => {
        await new Promise((r) => setTimeout(r, 5000))
        return HttpResponse.json({})
      }),
    )
    const { container } = render(<TaskDetail taskId="test-task" />)
    const skeletons = container.querySelectorAll('[data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('renders a permission failure instead of "Task not found" when the task 403s and stops dependent polling', async () => {
    mockSearch.current = { tab: 'runtime' }
    const hits: Record<string, number> = {}
    const forbidden = (key: string) => () => {
      hits[key] = (hits[key] ?? 0) + 1
      return HttpResponse.json({ error: { code: 403, message: 'scope missing' } }, { status: 403 })
    }
    server.use(
      http.get('/api/v1/tasks/:id', forbidden('task')),
      http.get('/api/v1/tasks/:id/events', forbidden('events')),
      http.get('/api/v1/tasks/:id/trace', forbidden('trace')),
      http.get('/api/v1/tasks/:id/approvals', forbidden('approvals')),
      http.get('/api/v1/tasks/:id/artifacts', forbidden('artifacts')),
    )
    render(<TaskDetail taskId="test-task" />)
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('Not authorized to view this task')
    })
    expect(screen.getByText(/scope missing/)).toBeInTheDocument()
    expect(screen.queryByText('Task not found')).not.toBeInTheDocument()
    // Nothing keeps polling once access is forbidden: no task retries and no
    // hidden dependent 403 traffic beyond a single request that raced the lookup.
    await new Promise((resolve) => setTimeout(resolve, 300))
    const snapshot = { ...hits }
    await new Promise((resolve) => setTimeout(resolve, 200))
    expect(hits).toEqual(snapshot)
    expect(hits.task).toBe(1)
    expect(hits.events ?? 0).toBeLessThanOrEqual(1)
    expect(hits.trace ?? 0).toBeLessThanOrEqual(1)
    expect(hits.approvals ?? 0).toBeLessThanOrEqual(1)
    expect(hits.artifacts ?? 0).toBeLessThanOrEqual(1)
  })

  it('stops polling the task and its dependent endpoints once the task 404s', async () => {
    mockSearch.current = { tab: 'runtime' }
    const hits: Record<string, number> = {}
    const count = (key: string) => {
      hits[key] = (hits[key] ?? 0) + 1
    }
    server.use(
      http.get('/api/v1/tasks/:id', () => {
        count('task')
        return HttpResponse.json({ error: { code: 404, message: 'task not found' } }, { status: 404 })
      }),
      http.get('/api/v1/tasks/:id/events', () => {
        count('events')
        return HttpResponse.json({ error: { code: 404, message: 'task not found' } }, { status: 404 })
      }),
      http.get('/api/v1/tasks/:id/trace', () => {
        count('trace')
        return HttpResponse.json({ error: { code: 404, message: 'task not found' } }, { status: 404 })
      }),
      http.get('/api/v1/tasks/:id/approvals', () => {
        count('approvals')
        return HttpResponse.json({ error: { code: 404, message: 'task not found' } }, { status: 404 })
      }),
      http.get('/api/v1/tasks/:id/artifacts', () => {
        count('artifacts')
        return HttpResponse.json({ error: { code: 404, message: 'task not found' } }, { status: 404 })
      }),
    )
    render(<TaskDetail taskId="nonexistent" />)
    await waitFor(() => {
      expect(screen.getByText('Task not found')).toBeInTheDocument()
    })
    // Let the trace hook's single 100ms retry settle, then confirm nothing
    // else fires: no 5s polling, no dependent refetches.
    await new Promise((resolve) => setTimeout(resolve, 300))
    const snapshot = { ...hits }
    await new Promise((resolve) => setTimeout(resolve, 200))
    expect(hits).toEqual(snapshot)
    expect(hits.task).toBe(1)
    // A 404 is never retried: at most the single in-flight request that
    // raced the task lookup.
    expect(hits.events ?? 0).toBeLessThanOrEqual(1)
    expect(hits.trace ?? 0).toBeLessThanOrEqual(1)
    expect(hits.artifacts ?? 0).toBeLessThanOrEqual(1)
    expect(hits.approvals ?? 0).toBeLessThanOrEqual(1)
    expect(hits.artifacts ?? 0).toBeLessThanOrEqual(1)
  })

  it('shows not-found when task does not exist', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () => new HttpResponse(null, { status: 404 })),
    )
    render(<TaskDetail taskId="nonexistent" />)
    await waitFor(() => {
      expect(screen.getByText('Task not found')).toBeInTheDocument()
    })
  })

  it('renders not-found once a loaded task 404s on refetch, ignoring the cached copy', async () => {
    let hits = 0
    server.use(
      http.get('/api/v1/tasks/:id', () => {
        hits++
        if (hits === 1) {
          return HttpResponse.json({
            metadata: { name: 'gone-task', namespace: 'default', uid: 'uid-gone' },
            spec: { type: 'container', image: 'alpine' },
            status: { phase: 'Succeeded' },
          })
        }
        return HttpResponse.json({ error: { code: 404, message: 'task not found' } }, { status: 404 })
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
    renderRaw(
      <QueryClientProvider client={queryClient}>
        <TaskDetail taskId="gone-task" />
      </QueryClientProvider>,
    )
    await waitFor(() => expect(screen.getByText('gone-task')).toBeInTheDocument())

    await queryClient.refetchQueries({ queryKey: ['task', 'gone-task'] })
    await waitFor(() => expect(screen.getByText('Task not found')).toBeInTheDocument())
    expect(screen.queryByText('gone-task')).not.toBeInTheDocument()
  })

  it('shows the Agent link for AI tasks created from an agentRef', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'ai-agent-task', namespace: 'default', uid: 'uid-ai' },
          spec: { type: 'ai', agentRef: { name: 'native-agent' }, ai: { prompt: 'Summarize' } },
          status: { phase: 'Pending' },
        }),
      ),
    )
    render(<TaskDetail taskId="ai-agent-task" />)
    await waitFor(() => expect(screen.getByText('AI Config')).toBeInTheDocument())
    const link = screen.getByRole('link', { name: 'native-agent' })
    expect(link).toHaveAttribute('href', expect.stringContaining('/agents/'))
    expect(screen.getAllByText('Agent default')).toHaveLength(2)
    expect(screen.getByText('Summarize')).toBeInTheDocument()
  })

  it('shows a cross-namespace agentRef qualified instead of linking into the wrong namespace', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'ai-xns-task', namespace: 'default', uid: 'uid-xns' },
          spec: { type: 'ai', agentRef: { name: 'shared-agent', namespace: 'platform' }, ai: { prompt: 'Summarize' } },
          status: { phase: 'Pending' },
        }),
      ),
    )
    render(<TaskDetail taskId="ai-xns-task" />)
    await waitFor(() => expect(screen.getByText('AI Config')).toBeInTheDocument())
    expect(screen.queryByRole('link', { name: 'shared-agent' })).not.toBeInTheDocument()
    expect(screen.getByText('platform/shared-agent')).toBeInTheDocument()
  })

  it('overview tab shows metadata', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'my-task', namespace: 'default', uid: 'uid-123', creationTimestamp: new Date().toISOString() },
          spec: { type: 'container', image: 'alpine' },
          status: { phase: 'Succeeded', attempts: 2 },
        }),
      ),
    )
    render(<TaskDetail taskId="my-task" />)
    await waitFor(() => {
      expect(screen.getByText('my-task')).toBeInTheDocument()
    })
    expect(screen.getByText('Metadata')).toBeInTheDocument()
    expect(screen.getByText('uid-123')).toBeInTheDocument()
    expect(screen.getByText('2')).toBeInTheDocument() // attempts
  })

  it('shows container config for container type', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'ct', namespace: 'default', uid: 'uid-ct' },
          spec: { type: 'container', image: 'nginx:latest', command: ['echo', 'hello'] },
          status: { phase: 'Running' },
        }),
      ),
    )
    render(<TaskDetail taskId="ct" />)
    await waitFor(() => {
      expect(screen.getByText('Container Config')).toBeInTheDocument()
    })
    expect(screen.getByText('nginx:latest')).toBeInTheDocument()
    expect(screen.getByText('echo hello')).toBeInTheDocument()
  })

  it('shows AI config for ai type', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'ai-task', namespace: 'default', uid: 'uid-ai' },
          spec: { type: 'ai', ai: { provider: 'anthropic', model: 'claude-sonnet-4-20250514', prompt: 'Hello AI' } },
          status: { phase: 'Succeeded' },
        }),
      ),
    )
    render(<TaskDetail taskId="ai-task" />)
    await waitFor(() => {
      expect(screen.getByText('AI Config')).toBeInTheDocument()
    })
    expect(screen.getByText('anthropic')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet-4-20250514')).toBeInTheDocument()
    expect(screen.getByText('Hello AI')).toBeInTheDocument()
  })

  it('shows agent config for agent type', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'agent-task', namespace: 'default', uid: 'uid-agent' },
          spec: { type: 'agent', agentRef: { name: 'my-agent' }, prompt: 'Do something' },
          status: { phase: 'Pending' },
        }),
      ),
    )
    render(<TaskDetail taskId="agent-task" />)
    await waitFor(() => {
      expect(screen.getByText('Agent Config')).toBeInTheDocument()
    })
    expect(screen.getByText('my-agent')).toBeInTheDocument()
    expect(screen.getByText('Do something')).toBeInTheDocument()
  })

  it('renders tabs for overview, result, and logs', async () => {
    render(<TaskDetail taskId="test-task" />)
    await waitFor(() => {
      expect(screen.getByText('Overview')).toBeInTheDocument()
    })
    expect(screen.getByText('Runtime')).toBeInTheDocument()
    expect(screen.getByText('Result')).toBeInTheDocument()
    expect(screen.getByText('Logs')).toBeInTheDocument()
  })

  it('switches to the Runtime tab and shows runtime panels', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'rt-task', namespace: 'default', uid: 'uid-rt' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )
    render(<TaskDetail taskId="rt-task" />)
    await waitFor(() => expect(screen.getByText('rt-task')).toBeInTheDocument())
    await user.click(screen.getByRole('tab', { name: /runtime/i }))
    expect(await screen.findByText('Task flow')).toBeInTheDocument()
    expect(screen.getByText('Derived checks')).toBeInTheDocument()
  })

  it('surfaces task event fetch failures in the Runtime timeline', async () => {
    mockSearch.current = { tab: 'runtime' }
    server.use(
      http.get('/api/v1/tasks/:id/events', () =>
        HttpResponse.json({ error: 'backend unavailable' }, { status: 500 }),
      ),
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'event-error', namespace: 'default', uid: 'uid-event-error' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )

    render(<TaskDetail taskId="event-error" />)

    expect(await screen.findByRole('alert')).toHaveTextContent('Unable to load events')
    expect(screen.queryByText('No events')).not.toBeInTheDocument()
  })

  it('surfaces disabled execution-event storage in the Runtime timeline', async () => {
    mockSearch.current = { tab: 'runtime' }
    server.use(
      http.get('/api/v1/tasks/:id/events', () =>
        HttpResponse.json({ error: 'execution events disabled' }, { status: 501 }),
      ),
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'unsupported-events', namespace: 'default', uid: 'uid-unsupported' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )

    render(<TaskDetail taskId="unsupported-events" />)

    expect(await screen.findByText('Live stream not enabled')).toBeInTheDocument()
    expect(screen.queryByText('No events')).not.toBeInTheDocument()
  })

  it('falls back to runtime tab when ?tab names an unavailable panel', async () => {
    mockSearch.current = { tab: 'children' } // task has no children → no children panel
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'no-kids', namespace: 'default', uid: 'uid-nk' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )
    render(<TaskDetail taskId="no-kids" />)
    // No blank body: runtime panels render instead of an empty children panel.
    expect(await screen.findByText('Task flow')).toBeInTheDocument()
  })

  it('delete button removes task and navigates', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'del-task', namespace: 'default', uid: 'uid-del' },
          spec: { type: 'container', image: 'alpine' },
          status: { phase: 'Succeeded' },
        }),
      ),
    )
    render(<TaskDetail taskId="del-task" />)
    await waitFor(() => {
      expect(screen.getByText('del-task')).toBeInTheDocument()
    })
    await user.click(screen.getByRole('button', { name: /^delete/i }))
    await user.click(screen.getByRole('button', { name: /confirm delete/i }))
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledWith({ to: '/tasks' })
    })
  })

  it('shows conditions when present', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'cond-task', namespace: 'default', uid: 'uid-cond', creationTimestamp: new Date().toISOString() },
          spec: { type: 'container', image: 'alpine' },
          status: {
            phase: 'Running',
            conditions: [
              { type: 'Ready', status: 'True', message: 'All good' },
              { type: 'Scheduled', status: 'False' },
            ],
          },
        }),
      ),
    )
    render(<TaskDetail taskId="cond-task" />)
    await waitFor(() => {
      expect(screen.getByText('Conditions')).toBeInTheDocument()
    })
    expect(screen.getByText('Ready')).toBeInTheDocument()
    expect(screen.getByText('All good', { exact: false })).toBeInTheDocument()
    expect(screen.getByText('Scheduled')).toBeInTheDocument()
  })

  it('timeAgo covers minutes, hours, and days', async () => {
    const now = Date.now()
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: {
            name: 'time-task', namespace: 'default', uid: 'uid-time',
            creationTimestamp: new Date(now - 120_000).toISOString(),
          },
          spec: { type: 'container', image: 'alpine' },
          status: {
            phase: 'Succeeded',
            startTime: new Date(now - 7200_000).toISOString(),
            completionTime: new Date(now - 172800_000).toISOString(),
          },
        }),
      ),
    )
    render(<TaskDetail taskId="time-task" />)
    await waitFor(() => {
      expect(screen.getByText('time-task')).toBeInTheDocument()
    })
    // 120s → "2m ago", 7200s → "2h ago", 172800s → "2d ago"
    expect(screen.getByText('2m ago')).toBeInTheDocument()
    expect(screen.getByText('2h ago')).toBeInTheDocument()
    expect(screen.getByText('2d ago')).toBeInTheDocument()
  })

  it('renders the execution graph (not a table) in the Children tab', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'parent', namespace: 'default', uid: 'uid-p', creationTimestamp: new Date().toISOString() },
          spec: { type: 'agent', agentRef: { name: 'orchestrator' } },
          status: {
            phase: 'Running',
            childTasks: [
              { name: 'child-1', agent: 'reviewer', phase: 'Succeeded' },
              { name: 'child-2', agent: 'fixer', phase: 'Running' },
            ],
          },
        }),
      ),
    )
    render(<TaskDetail taskId="parent" />)
    await waitFor(() => expect(screen.getByText('parent')).toBeInTheDocument())
    await userEvent.click(screen.getByRole('tab', { name: /children/i }))
    // Execution graph (role=tree) replaces the old child-tasks table.
    expect(await screen.findByRole('tree', { name: /execution graph/i })).toBeInTheDocument()
    expect(screen.getByText('child-1')).toBeInTheDocument()
    expect(screen.getByText('child-2')).toBeInTheDocument()
  })

  it('renders the run timeline in the Plan tab when iterating', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'auto', namespace: 'default', uid: 'uid-a', creationTimestamp: new Date().toISOString() },
          spec: { type: 'agent', agentRef: { name: 'looper' } },
          status: { phase: 'Running', iteration: 3, startTime: new Date().toISOString() },
          plan: { summary: 'converging on the goal', progressPct: 66 },
        }),
      ),
    )
    render(<TaskDetail taskId="auto" />)
    await waitFor(() => expect(screen.getByText('auto')).toBeInTheDocument())
    await userEvent.click(screen.getByRole('tab', { name: /plan/i }))
    // RunTimeline shows the iteration + plan summary + progress bar.
    expect(await screen.findAllByText('Iteration 3')).toHaveLength(1)
    expect(screen.getAllByText('converging on the goal').length).toBeGreaterThan(0)
    expect(screen.getAllByRole('progressbar', { name: /goal progress/i }).length).toBeGreaterThan(0)
  })

  it('does not show the Plan tab when no plan is persisted', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'noiter', namespace: 'default', uid: 'uid-n', creationTimestamp: new Date().toISOString() },
          spec: { type: 'container', image: 'alpine' },
          status: { phase: 'Running', iteration: 0 },
        }),
      ),
    )
    render(<TaskDetail taskId="noiter" />)
    await waitFor(() => expect(screen.getByText('noiter')).toBeInTheDocument())
    expect(screen.queryByRole('tab', { name: /plan/i })).not.toBeInTheDocument()
  })

  it('keeps autonomous run history visible after the persisted plan is cleaned up', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'completed-auto', namespace: 'default', uid: 'uid-c', creationTimestamp: new Date().toISOString() },
          spec: { type: 'agent', agentRef: { name: 'looper' } },
          status: { phase: 'Succeeded', iteration: 3, completionTime: new Date().toISOString() },
        }),
      ),
    )
    render(<TaskDetail taskId="completed-auto" />)
    await waitFor(() => expect(screen.getByText('completed-auto')).toBeInTheDocument())
    await userEvent.click(screen.getByRole('tab', { name: /plan/i }))
    expect(await screen.findByText('Iteration 3')).toBeInTheDocument()
  })

  it('keeps iteration-zero plan history visible from durable events', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'completed-agent', namespace: 'default', uid: 'uid-e', creationTimestamp: new Date().toISOString() },
          spec: { type: 'agent', agentRef: { name: 'planner' } },
          status: { phase: 'Succeeded', iteration: 0, completionTime: new Date().toISOString() },
        }),
      ),
      http.get('/api/v1/tasks/:id/events', ({ params }) =>
        HttpResponse.json({
          namespace: 'default',
          streamType: 'task',
          streamID: params.id,
          afterSeq: 0,
          latestSeq: 2,
          events: [
            {
              id: 'plan-1',
              namespace: 'default',
              streamType: 'task',
              streamID: params.id,
              seq: 1,
              type: 'PlanUpdated',
              severity: 'info',
              summary: 'Plan updated (0/1 steps complete)',
              content: { progressPct: 0, goalComplete: false },
              contentText: '# Plan\n- [ ] inspect',
              createdAt: new Date().toISOString(),
            },
            {
              id: 'plan-2',
              namespace: 'default',
              streamType: 'task',
              streamID: params.id,
              seq: 2,
              type: 'PlanUpdated',
              severity: 'info',
              summary: 'Plan in progress (1/2 complete): verify',
              content: { progressPct: 50, goalComplete: false },
              contentText: '# Plan\n- [x] inspect\n- [ ] verify _(in progress)_',
              createdAt: new Date().toISOString(),
            },
          ],
        }),
      ),
    )
    render(<TaskDetail taskId="completed-agent" />)
    await waitFor(() => expect(screen.getByText('completed-agent')).toBeInTheDocument())
    const planTab = await screen.findByRole('tab', { name: /plan/i })
    await userEvent.click(planTab)
    expect(await screen.findByText('Agent Plan')).toBeInTheDocument()
    expect(screen.getByRole('progressbar', { name: /goal progress/i })).toHaveAttribute('aria-valuenow', '50')
    expect(screen.getByText('Plan document')).toBeInTheDocument()
    const document = screen.getByText((_, element) => element?.tagName.toLowerCase() === 'pre')
    expect(document).toHaveTextContent('- [x] inspect')
    expect(document).toHaveTextContent('- [ ] verify')
  })

  it('shows a persisted plan when iteration is 0', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'planned', namespace: 'default', uid: 'uid-p', creationTimestamp: new Date().toISOString() },
          spec: { type: 'agent', agentRef: { name: 'planner' } },
          status: { phase: 'Running', iteration: 0 },
          plan: { summary: 'inspect and verify', progressPct: 25, planDocument: '# Plan\n- inspect' },
        }),
      ),
    )
    render(<TaskDetail taskId="planned" />)
    await waitFor(() => expect(screen.getByText('planned')).toBeInTheDocument())
    await userEvent.click(screen.getByRole('tab', { name: /plan/i }))
    expect(await screen.findByRole('progressbar', { name: /goal progress/i })).toBeInTheDocument()
    expect(screen.getByText('Plan document')).toBeInTheDocument()
    const document = screen.getByText((_, element) => element?.tagName.toLowerCase() === 'pre')
    expect(document).toHaveTextContent('# Plan')
    expect(document).toHaveTextContent('- inspect')
  })
})
