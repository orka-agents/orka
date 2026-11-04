import { describe, it, expect, beforeEach, vi } from 'vitest'
import { act, render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockNavigate = vi.fn()
const mockSearch: { current: { tab?: string } } = { current: { tab: 'spec' } }
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

import { toast } from 'sonner'
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
    // Most tests read the Task resource, which the Spec tab shows.
    mockSearch.current = { tab: 'spec' }
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
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
    mockSearch.current = { tab: 'overview' }
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
    // A titled page like the other detail views: heading, tab title, back link.
    expect(screen.getByRole('heading', { level: 1, name: 'Could not load task' })).toBeInTheDocument()
    expect(document.title).toBe('Could not load task · Orka')
    expect(screen.getByRole('link', { name: /back to tasks/i })).toHaveAttribute('href', '/tasks')
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
    mockSearch.current = { tab: 'overview' }
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
    // Dependent panels wait for the task to load, so a task that never
    // existed generates no events/trace/approvals/artifacts requests at all.
    expect(hits.events ?? 0).toBe(0)
    expect(hits.trace ?? 0).toBe(0)
    expect(hits.approvals ?? 0).toBe(0)
    expect(hits.artifacts ?? 0).toBe(0)
  })

  it('shows an error, not "Task not found", when the first load fails with a 500', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () => HttpResponse.json({ error: { code: 500, message: 'store unavailable' } }, { status: 500 })),
    )
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      render(<TaskDetail taskId="broken" />)
      // The detail read retries a failed load three times before giving up.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000)
      })
    } finally {
      vi.useRealTimers()
    }
    expect(await screen.findByText('The task could not be loaded')).toBeInTheDocument()
    expect(screen.queryByText('Task not found')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: /back to tasks/i })).toHaveAttribute('href', '/tasks')
  })

  it('keeps the load-error page, not the skeleton, while a failed first load polls again', async () => {
    let failing = true
    let hits = 0
    server.use(
      http.get('/api/v1/tasks/:id', () => {
        hits += 1
        return failing
          ? HttpResponse.json({ error: { code: 500, message: 'store unavailable' } }, { status: 500 })
          : HttpResponse.json({
            metadata: { name: 'broken', namespace: 'default', uid: 'uid-broken' },
            spec: { type: 'container', image: 'alpine' },
            status: { phase: 'Succeeded' },
          })
      }),
    )
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const { container } = render(<TaskDetail taskId="broken" />)
      // The first load and its three retries (1s, 2s, 4s backoff) settle into the error page.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(8_000)
      })
      expect(screen.getByText('The task could not be loaded')).toBeInTheDocument()
      const settledHits = hits
      // The 5s poll (re-armed when the error landed) starts another load
      // that is still retrying: the error page and its back link stay up
      // instead of the loading skeleton.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_500)
      })
      expect(hits).toBeGreaterThan(settledHits)
      expect(screen.getByText('The task could not be loaded')).toBeInTheDocument()
      expect(screen.getByRole('link', { name: /back to tasks/i })).toBeInTheDocument()
      expect(container.querySelector('[data-slot="skeleton"]')).toBeNull()
      // A later successful load replaces the error page with the task.
      failing = false
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000)
      })
    } finally {
      vi.useRealTimers()
    }
    expect(await screen.findByRole('heading', { name: 'broken' })).toBeInTheDocument()
    expect(screen.queryByText('The task could not be loaded')).not.toBeInTheDocument()
  })

  it('shows not-found when task does not exist', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () => new HttpResponse(null, { status: 404 })),
    )
    render(<TaskDetail taskId="nonexistent" />)
    await waitFor(() => {
      expect(screen.getByText('Task not found')).toBeInTheDocument()
    })
    expect(screen.getByText(/No task named "nonexistent" exists in this namespace/)).toBeInTheDocument()
    // Like the 403/500 pages, the missing-task page has a top-level heading.
    expect(screen.getByRole('heading', { level: 1, name: 'Task not found' })).toBeInTheDocument()
    const back = screen.getByRole('link', { name: /back to tasks/i })
    expect(back).toHaveAttribute('href', '/tasks')
    expect(back.querySelector('button')).toBeNull()
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

  it('spec tab shows metadata', async () => {
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
    // One named link, not a button nested in a link.
    const back = screen.getByRole('link', { name: 'Back to tasks' })
    expect(back).toHaveAttribute('href', '/tasks')
    expect(back.querySelector('button')).toBeNull()
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

  it('shows a container command and args shell-quoted so each argument reads unambiguously', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'retry', namespace: 'default', uid: 'uid-retry' },
          spec: { type: 'container', image: 'alpine', command: ['sh', '-c', 'echo attempt; exit 1'], args: ['--name', "it's here"] },
          status: { phase: 'Failed' },
        }),
      ),
    )
    render(<TaskDetail taskId="retry" />)
    expect(await screen.findByText(`sh -c 'echo attempt; exit 1'`)).toBeInTheDocument()
    expect(screen.getByText(`--name 'it'\\''s here'`)).toBeInTheDocument()
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

  it('shows the Overview, Activity, Approvals, Result, Logs and Spec tabs', async () => {
    render(<TaskDetail taskId="test-task" />)
    const tabs = await screen.findAllByRole('tab')
    expect(tabs.map((tab) => tab.textContent)).toEqual(['Overview', 'Activity', 'Approvals', 'Result', 'Logs', 'Spec'])
  })

  it('opens on the Overview, with progress and derived checks', async () => {
    mockSearch.current = {}
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
    expect(await screen.findByText('Derived checks')).toBeInTheDocument()
    expect(screen.getByText('Execution')).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('data-state', 'active')
  })

  it('says on the Overview when execution events fail to load', async () => {
    mockSearch.current = { tab: 'overview' }
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

    expect(await screen.findByRole('alert')).toHaveTextContent('Unable to load execution events')
  })

  it('says on the Overview when execution-event storage is off', async () => {
    mockSearch.current = { tab: 'overview' }
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

    expect(await screen.findByText(/Execution events are not enabled/)).toBeInTheDocument()
  })

  it('falls back to the Overview when ?tab names an unavailable panel', async () => {
    mockSearch.current = { tab: 'plan' } // the task has no plan, so no Plan tab
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
    expect(await screen.findByText('Derived checks')).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('data-state', 'active')
  })

  it('sends links to merged tabs to the tab that now shows their content', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'rt-task', namespace: 'default', uid: 'uid-rt' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )
    for (const [tab, lands] of [['runtime', 'Overview'], ['execution', 'Overview'], ['children', 'Overview'], ['trace', 'Activity'], ['timeline', 'Activity']]) {
      mockSearch.current = { tab }
      const { unmount } = render(<TaskDetail taskId="rt-task" />)
      expect(await screen.findByRole('tab', { name: lands })).toHaveAttribute('data-state', 'active')
      if (tab === 'timeline') expect(screen.getByRole('button', { name: 'Raw events' })).toHaveAttribute('aria-pressed', 'true')
      if (tab === 'trace') expect(screen.getByRole('button', { name: 'Trace' })).toHaveAttribute('aria-pressed', 'true')
      unmount()
    }
  })

  it('switches Activity to raw events when a trace link becomes a timeline link', async () => {
    mockSearch.current = { tab: 'trace' }
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'rt-task', namespace: 'default', uid: 'uid-rt' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )
    const { rerender } = render(<TaskDetail taskId="rt-task" />)
    expect(await screen.findByRole('button', { name: 'Trace' })).toHaveAttribute('aria-pressed', 'true')
    mockSearch.current = { tab: 'timeline' }
    rerender(<TaskDetail taskId="rt-task" />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Raw events' })).toHaveAttribute('aria-pressed', 'true'))
  })

  it('fetches artifacts once more when the task finishes while Result is open', async () => {
    mockSearch.current = { tab: 'result' }
    let phase = 'Running'
    let artifactRequests = 0
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'finishing', namespace: 'default', uid: 'uid-fin' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase },
        }),
      ),
      http.get('/api/v1/tasks/:id/artifacts', () => {
        artifactRequests += 1
        return HttpResponse.json({ artifacts: [] })
      }),
    )
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    renderRaw(<QueryClientProvider client={client}><TaskDetail taskId="finishing" /></QueryClientProvider>)
    await waitFor(() => expect(artifactRequests).toBe(1))

    // The task finishes; refetch only the task, well inside the 5s poll.
    phase = 'Succeeded'
    await act(async () => {
      await client.invalidateQueries({ queryKey: ['task'] })
    })
    await waitFor(() => expect(artifactRequests).toBe(2))
  })

  it('shows activity as a trace or as raw events', async () => {
    const user = userEvent.setup()
    mockSearch.current = { tab: 'activity' }
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
    expect(await screen.findByText('Execution trace')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Raw events' }))
    expect(await screen.findByText('Execution timeline')).toBeInTheDocument()
    expect(screen.queryByText('Execution trace')).not.toBeInTheDocument()
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

  it('keeps the page and explains a failed delete instead of hanging on Confirm', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'locked-task', namespace: 'default', uid: 'uid-locked' },
          spec: { type: 'container', image: 'alpine' },
          status: { phase: 'Succeeded' },
        }),
      ),
      http.delete('/api/v1/tasks/:id', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized to delete tasks' } }, { status: 403 }),
      ),
    )
    render(<TaskDetail taskId="locked-task" />)
    await user.click(await screen.findByRole('button', { name: /^delete/i }))
    await user.click(screen.getByRole('button', { name: /confirm delete/i }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete task: not authorized to delete tasks'))
    expect(mockNavigate).not.toHaveBeenCalled()
    // The confirm is disarmed so the header is usable again.
    expect(screen.queryByRole('button', { name: /confirm delete/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^delete/i })).toBeEnabled()
    // Focus returns to Delete instead of dropping to the page.
    expect(screen.getByRole('button', { name: /^delete/i })).toHaveFocus()
  })

  it('moves focus into the inline delete confirm and back out on Cancel or Esc', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'focus-task', namespace: 'default', uid: 'uid-focus' },
          spec: { type: 'container', image: 'alpine' },
          status: { phase: 'Succeeded' },
        }),
      ),
    )
    render(<TaskDetail taskId="focus-task" />)
    const deleteButton = await screen.findByRole('button', { name: /^delete/i })
    // The task loaded with focus on the page, and arming nothing moved it.
    expect(deleteButton).not.toHaveFocus()

    await user.click(deleteButton)
    expect(screen.getByRole('button', { name: /confirm delete/i })).toHaveFocus()
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('button', { name: /confirm delete/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^delete/i })).toHaveFocus()

    await user.keyboard('{Enter}')
    expect(screen.getByRole('button', { name: /confirm delete/i })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('button', { name: /confirm delete/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^delete/i })).toHaveFocus()
  })

  it('counts pending approvals on the Approvals tab while the task waits', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'gated', namespace: 'default', uid: 'uid-gated' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True' }] },
        }),
      ),
      http.get('/api/v1/tasks/:id/approvals', () =>
        HttpResponse.json({
          namespace: 'default',
          taskName: 'gated',
          approvals: [
            { id: 'a1', action: 'bash', status: 'pending', createdAt: '2026-01-01T00:00:00Z' },
            { id: 'a2', action: 'write', status: 'pending', createdAt: '2026-01-01T00:00:00Z' },
            { id: 'a3', action: 'read', status: 'approved', createdAt: '2026-01-01T00:00:00Z' },
          ],
        }),
      ),
    )
    render(<TaskDetail taskId="gated" />)
    expect(await screen.findByRole('tab', { name: 'Approvals 2 pending' })).toBeInTheDocument()
    expect(screen.getAllByText('Waiting for approval').length).toBeGreaterThan(0)
  })

  it('shows a Deleting state instead of an active Delete for a task being deleted', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'going', namespace: 'default', uid: 'uid-going', deletionTimestamp: '2026-10-14T00:00:00Z' },
          spec: { type: 'container', image: 'alpine' },
          status: { phase: 'Running' },
        }),
      ),
    )
    render(<TaskDetail taskId="going" />)
    expect(await screen.findByRole('button', { name: /deleting/i })).toBeDisabled()
    expect(screen.queryByRole('button', { name: /^delete$/i })).not.toBeInTheDocument()
    expect(screen.getByText('Deleting', { selector: '[data-slot="badge"]' })).toBeInTheDocument()
  })

  it('names the browser tab "Task not found" for a missing task', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({ error: { code: 404, message: 'task not found' } }, { status: 404 }),
      ),
    )
    const { unmount } = render(<TaskDetail taskId="gone" />)
    await screen.findByText('Task not found')
    expect(document.title).toBe('Task not found · Orka')
    unmount()
  })

  it('scrolls the sideways-scrolling tab bar to bring the selected tab into view', async () => {
    const rect = (left: number, right: number) => ({ left, right, top: 0, bottom: 20, width: right - left, height: 20, x: left, y: 0, toJSON: () => ({}) }) as DOMRect
    const scrollWidth = vi.spyOn(Element.prototype, 'scrollWidth', 'get').mockReturnValue(600)
    const clientWidth = vi.spyOn(Element.prototype, 'clientWidth', 'get').mockReturnValue(300)
    // The tab bar spans 0-300px; the Logs tab sits past its right edge.
    const bounds = vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
      if (this.getAttribute('role') === 'tab') return this.textContent === 'Logs' ? rect(400, 460) : rect(10, 70)
      return rect(0, 300)
    })
    try {
      const user = userEvent.setup()
      render(<TaskDetail taskId="test-task" />)
      const logs = await screen.findByRole('tab', { name: 'Logs' })
      const scroller = screen.getByRole('tablist').parentElement!
      // The initially active Overview tab is already in view.
      expect(scroller.scrollLeft).toBe(0)
      await user.click(logs)
      await waitFor(() => expect(scroller.scrollLeft).toBe(160))
    } finally {
      scrollWidth.mockRestore()
      clientWidth.mockRestore()
      bounds.mockRestore()
    }
  })

  it('keeps the Approvals tab in view once its pending badge widens it', async () => {
    mockSearch.current = { tab: 'approvals' }
    const rect = (left: number, right: number) => ({ left, right, top: 0, bottom: 20, width: right - left, height: 20, x: left, y: 0, toJSON: () => ({}) }) as DOMRect
    const scrollWidth = vi.spyOn(Element.prototype, 'scrollWidth', 'get').mockReturnValue(600)
    const clientWidth = vi.spyOn(Element.prototype, 'clientWidth', 'get').mockReturnValue(300)
    // The tab bar spans 0-300px. The Approvals tab fits until the "2 pending"
    // badge renders and pushes its right edge to 340px.
    const bounds = vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
      if (this.getAttribute('role') === 'tab') {
        if (!this.textContent?.startsWith('Approvals')) return rect(10, 70)
        return this.textContent.includes('pending') ? rect(250, 340) : rect(250, 290)
      }
      return rect(0, 300)
    })
    let releaseApprovals!: () => void
    const approvalsReady = new Promise<void>((resolve) => { releaseApprovals = resolve })
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: { name: 'gated', namespace: 'default', uid: 'uid-gated' },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True' }] },
        }),
      ),
      http.get('/api/v1/tasks/:id/approvals', async () => {
        await approvalsReady
        return HttpResponse.json({
          namespace: 'default',
          taskName: 'gated',
          approvals: [
            { id: 'a1', action: 'bash', status: 'pending', createdAt: '2026-01-01T00:00:00Z' },
            { id: 'a2', action: 'write', status: 'pending', createdAt: '2026-01-01T00:00:00Z' },
          ],
        })
      }),
    )
    try {
      render(<TaskDetail taskId="gated" />)
      await screen.findByRole('tab', { name: 'Approvals' })
      const scroller = screen.getByRole('tablist').parentElement!
      // Without the badge the selected tab is already in view.
      expect(scroller.scrollLeft).toBe(0)
      releaseApprovals()
      await screen.findByRole('tab', { name: 'Approvals 2 pending' })
      await waitFor(() => expect(scroller.scrollLeft).toBe(40))
    } finally {
      releaseApprovals()
      scrollWidth.mockRestore()
      clientWidth.mockRestore()
      bounds.mockRestore()
    }
  })

  it('links a child task back to its parent', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: {
            name: 'child-task',
            namespace: 'default',
            uid: 'uid-child',
            labels: { 'orka.ai/parent-task': 'parent-hash' },
            annotations: { 'orka.ai/parent-task-name': 'the-parent' },
          },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )
    render(<TaskDetail taskId="child-task" />)
    const link = await screen.findByRole('link', { name: 'the-parent' })
    expect(link).toHaveAttribute('href', '/tasks/$taskId')
    expect(link.closest('p')).toHaveTextContent('Child of the-parent')
  })

  it('says a fork was forked from its source instead of calling it a child', async () => {
    server.use(
      http.get('/api/v1/tasks/:id', () =>
        HttpResponse.json({
          metadata: {
            name: 'fork-task',
            namespace: 'default',
            uid: 'uid-fork',
            labels: { 'orka.ai/parent-task': 'source-task' },
            annotations: { 'orka.ai/parent-task-name': 'source-task', 'orka.ai/fork-source-task': 'source-task' },
          },
          spec: { type: 'agent', agentRef: { name: 'a' } },
          status: { phase: 'Running' },
        }),
      ),
    )
    render(<TaskDetail taskId="fork-task" />)
    await screen.findByTestId('fork-provenance')
    const header = screen.getAllByRole('link', { name: 'source-task' }).map((link) => link.closest('p')).find(Boolean)
    expect(header).toHaveTextContent('Forked from source-task')
    expect(screen.queryByText(/Child of/)).not.toBeInTheDocument()
  })

  it('has a single Delete control on the Overview', async () => {
    mockSearch.current = { tab: 'overview' }
    render(<TaskDetail taskId="test-task" />)
    expect(await screen.findByText('Derived checks')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: /delete/i })).toHaveLength(1)
  })

  it('switches the Spec tab between the summary and the Task as YAML', async () => {
    const user = userEvent.setup()
    render(<TaskDetail taskId="test-task" />)
    expect(await screen.findByText('Metadata')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'YAML' }))
    expect(await screen.findByRole('region', { name: 'Task YAML' })).toHaveTextContent('kind: Task')
    expect(screen.queryByText('Metadata')).not.toBeInTheDocument()
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

  it('draws child tasks as an execution graph on the Overview', async () => {
    mockSearch.current = { tab: 'overview' }
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
    expect(await screen.findByRole('tree', { name: /execution graph/i })).toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: /children/i })).not.toBeInTheDocument()
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
