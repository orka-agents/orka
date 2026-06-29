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

  it('shows not-found when task does not exist', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () => new HttpResponse(null, { status: 404 })),
    )
    render(<TaskDetail taskId="nonexistent" />)
    await waitFor(() => {
      expect(screen.getByText('Task not found')).toBeInTheDocument()
    })
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

  it('does not show the Plan tab when iteration is 0', async () => {
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
})
