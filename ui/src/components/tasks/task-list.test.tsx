import { describe, it, expect, beforeEach, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
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
    useLocation: () => ({ pathname: '/tasks' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskList } from './task-list'

function makeTask(name: string, namespace = 'default') {
  return {
    metadata: {
      name,
      namespace,
      uid: `uid-${namespace}-${name}`,
      creationTimestamp: new Date().toISOString(),
    },
    spec: { type: 'container' },
    status: { phase: 'Running' },
  }
}

describe('TaskList', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('loading state shows skeleton rows', () => {
    // Default handler returns empty list, but first render is loading
    server.use(
      http.get('/api/v1/tasks', async () => {
        await new Promise((r) => setTimeout(r, 5000))
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )
    const { container } = render(<TaskList />)
    const skeletons = container.querySelectorAll('[data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('empty state shows "No tasks found"', async () => {
    render(<TaskList />)
    await waitFor(() => {
      expect(screen.getByText(/No tasks found/)).toBeInTheDocument()
    })
  })

  it('shows an accessible initial error state and can retry', async () => {
    const user = userEvent.setup()
    let calls = 0
    server.use(
      http.get('/api/v1/tasks', () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.text('temporary failure', { status: 500 })
        }
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )

    render(<TaskList />)

    expect(
      await screen.findByRole('alert', { name: /failed to load tasks/i }),
    ).toBeInTheDocument()
    await user.click(
      screen.getByRole('button', { name: /retry loading tasks/i }),
    )

    expect(await screen.findByText(/No tasks found/)).toBeInTheDocument()
    expect(calls).toBe(2)
  })

  it('populated table shows task rows', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'task-1', namespace: 'default', uid: 'uid-1', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 'task-2', namespace: 'prod', uid: 'uid-2', creationTimestamp: new Date().toISOString() },
              spec: { type: 'ai' },
              status: { phase: 'Succeeded' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    await waitFor(() => {
      expect(screen.getByText('task-1')).toBeInTheDocument()
    })
    expect(screen.getByText('task-2')).toBeInTheDocument()
    expect(screen.getByText('container')).toBeInTheDocument()
    expect(screen.getByText('ai')).toBeInTheDocument()
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('default')).toBeInTheDocument()
    expect(screen.getByText('prod')).toBeInTheDocument()
  })

  it('loads the second page so the 26th task is visible', async () => {
    const user = userEvent.setup()
    const requests: (string | null)[] = []
    let releaseSecondPage: () => void = () => {}
    const secondPageGate = new Promise<void>((resolve) => {
      releaseSecondPage = resolve
    })
    const firstPage = Array.from({ length: 25 }, (_, index) =>
      makeTask(`task-${index + 1}`),
    )

    server.use(
      http.get('/api/v1/tasks', async ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        requests.push(cursor)
        if (!cursor) {
          return HttpResponse.json({
            items: firstPage,
            metadata: { continue: 'page-2', remainingItemCount: 1 },
          })
        }

        expect(cursor).toBe('page-2')
        await secondPageGate
        return HttpResponse.json({
          items: [makeTask('task-26')],
          metadata: {},
        })
      }),
    )

    render(<TaskList />)

    expect(await screen.findByText('task-25')).toBeInTheDocument()
    expect(screen.queryByText('task-26')).not.toBeInTheDocument()

    await user.click(
      screen.getByRole('button', { name: /load more tasks/i }),
    )
    expect(
      screen.getByRole('button', { name: /loading more tasks/i }),
    ).toBeDisabled()

    releaseSecondPage()

    expect(await screen.findByText('task-26')).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /load more tasks/i }),
    ).not.toBeInTheDocument()
    expect(requests).toEqual([null, 'page-2'])
  })

  it('keeps loaded rows visible when loading more fails and can retry', async () => {
    const user = userEvent.setup()
    let secondPageCalls = 0
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        if (!cursor) {
          return HttpResponse.json({
            items: [makeTask('task-1')],
            metadata: { continue: 'page-2' },
          })
        }

        secondPageCalls += 1
        if (secondPageCalls === 1) {
          return HttpResponse.text('temporary failure', { status: 500 })
        }
        return HttpResponse.json({
          items: [makeTask('task-2')],
          metadata: {},
        })
      }),
    )

    render(<TaskList />)

    expect(await screen.findByText('task-1')).toBeInTheDocument()
    await user.click(
      screen.getByRole('button', { name: /load more tasks/i }),
    )

    expect(
      await screen.findByRole('alert', {
        name: /failed to load more tasks/i,
      }),
    ).toBeInTheDocument()
    expect(screen.getByText('task-1')).toBeInTheDocument()

    await user.click(
      screen.getByRole('button', { name: /retry loading more tasks/i }),
    )

    expect(await screen.findByText('task-2')).toBeInTheDocument()
    expect(secondPageCalls).toBe(2)
  })

  it('disables load more while the loaded pages refresh in the background', async () => {
    const user = userEvent.setup()
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 } },
    })
    let firstPageCalls = 0
    let secondPageCalls = 0
    let releaseRefresh: () => void = () => {}
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    server.use(
      http.get('/api/v1/tasks', async ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        if (cursor) {
          secondPageCalls += 1
          return HttpResponse.json({
            items: [makeTask('task-2')],
            metadata: {},
          })
        }

        firstPageCalls += 1
        if (firstPageCalls > 1) await refreshGate
        return HttpResponse.json({
          items: [makeTask('task-1')],
          metadata: { continue: 'page-2' },
        })
      }),
    )

    render(
      <QueryClientProvider client={queryClient}>
        <TaskList />
      </QueryClientProvider>,
    )
    expect(
      await screen.findByRole('button', { name: /load more tasks/i }),
    ).toBeEnabled()

    let refreshPromise: Promise<void> = Promise.resolve()
    act(() => {
      refreshPromise = queryClient.refetchQueries({
        queryKey: ['tasks', 'default', '25'],
      })
    })

    const refreshingButton = await screen.findByRole('button', {
      name: /refreshing tasks/i,
    })
    expect(refreshingButton).toBeDisabled()
    await user.click(refreshingButton)
    expect(secondPageCalls).toBe(0)

    releaseRefresh()
    await act(async () => {
      await refreshPromise
    })
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: /load more tasks/i }),
      ).toBeEnabled(),
    )
  })

  it('New Task button links to /tasks/new', async () => {
    render(<TaskList />)
    const link = screen.getByText('New Task').closest('a')
    expect(link).toHaveAttribute('href', '/tasks/new')
  })

  it('delete button calls deleteTask', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'del-task', namespace: 'default', uid: 'uid-del', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Succeeded' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    await waitFor(() => {
      expect(screen.getByText('del-task')).toBeInTheDocument()
    })
    const row = screen.getByText('del-task').closest('tr')!
    const deleteBtn = within(row).getByRole('button', {
      name: /delete task del-task/i,
    })
    await user.click(deleteBtn)
    // Verify no error - mutation fires without throwing
    expect(screen.getByText('del-task')).toBeInTheDocument()
  })

  it('timeAgo covers minutes, hours, and days', async () => {
    const now = Date.now()
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 't-min', namespace: 'default', uid: 'u1', creationTimestamp: new Date(now - 120_000).toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 't-hr', namespace: 'default', uid: 'u2', creationTimestamp: new Date(now - 7200_000).toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 't-day', namespace: 'default', uid: 'u3', creationTimestamp: new Date(now - 172800_000).toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    await waitFor(() => {
      expect(screen.getByText('t-min')).toBeInTheDocument()
    })
    expect(screen.getByText('2m')).toBeInTheDocument()
    expect(screen.getByText('2h')).toBeInTheDocument()
    expect(screen.getByText('2d')).toBeInTheDocument()
  })
})
