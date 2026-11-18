import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { maxListWalkPages } from '@/lib/list-api'

const navigateMock = vi.hoisted(() => vi.fn())

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => navigateMock,
    useLocation: () => ({ pathname: '/tasks' }),
  }
})

import { toast } from 'sonner'

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskList } from './task-list'

describe('TaskList', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    vi.clearAllMocks()
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
    expect(screen.getByText('Container')).toBeInTheDocument()
    expect(screen.getByText('AI')).toBeInTheDocument()
    expect(screen.queryByText('Ai')).not.toBeInTheDocument()
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('default')).toBeInTheDocument()
    expect(screen.getByText('prod')).toBeInTheDocument()
  })

  it('New Task button links to /tasks/new', async () => {
    render(<TaskList />)
    const link = screen.getByText('New Task').closest('a')
    expect(link).toHaveAttribute('href', '/tasks/new')
  })

  function serveDeletableTask() {
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
  }

  it('asks for confirmation before deleting and deletes once on confirm', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    const deletes: string[] = []
    server.use(
      http.delete('/api/v1/tasks/:id', ({ params }) => {
        deletes.push(String(params.id))
        return new HttpResponse(null, { status: 204 })
      }),
    )
    render(<TaskList />)
    await user.click(await screen.findByRole('button', { name: 'Delete task del-task' }))
    // The trash icon only arms the delete; nothing is sent yet.
    expect(screen.getByRole('dialog')).toHaveTextContent('del-task')
    expect(deletes).toEqual([])

    await user.dblClick(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Task del-task deleted'))
    expect(deletes).toEqual(['del-task'])
    expect(navigateMock).not.toHaveBeenCalled()
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('cancelling the confirmation sends no delete', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    const deletes: string[] = []
    server.use(
      http.delete('/api/v1/tasks/:id', ({ params }) => {
        deletes.push(String(params.id))
        return new HttpResponse(null, { status: 204 })
      }),
    )
    render(<TaskList />)
    await user.click(await screen.findByRole('button', { name: 'Delete task del-task' }))
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(deletes).toEqual([])
  })

  it('returns focus to the row delete button when the confirmation closes', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    render(<TaskList />)
    const trash = await screen.findByRole('button', { name: 'Delete task del-task' })

    await user.click(trash)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await waitFor(() => expect(trash).toHaveFocus())

    await user.click(trash)
    await screen.findByRole('dialog')
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await waitFor(() => expect(trash).toHaveFocus())
  })

  it('shows the server message without a repeated prefix when the delete fails', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    server.use(
      http.delete('/api/v1/tasks/:id', () =>
        HttpResponse.json({ error: { code: 500, message: 'failed to delete task: etcd unavailable' } }, { status: 500 }),
      ),
    )
    render(<TaskList />)
    await user.click(await screen.findByRole('button', { name: 'Delete task del-task' }))
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete task: etcd unavailable'))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('flags waiting, deleting, and suspended tasks without extra requests', async () => {
    const requests: string[] = []
    server.use(
      http.get('/api/v1/tasks/:id/approvals', ({ params }) => {
        requests.push(String(params.id))
        return HttpResponse.json({ approvals: [] })
      }),
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            { metadata: { name: 'gated', namespace: 'default', uid: 'g' }, spec: { type: 'agent' }, status: { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True' }] } },
            { metadata: { name: 'going', namespace: 'default', uid: 'd', deletionTimestamp: new Date().toISOString() }, spec: { type: 'container' }, status: { phase: 'Running' } },
            { metadata: { name: 'nightly', namespace: 'default', uid: 's' }, spec: { type: 'container', schedule: '0 2 * * *', suspend: true }, status: { phase: 'Scheduled' } },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    expect(await screen.findByText('Waiting for approval')).toBeInTheDocument()
    expect(screen.getByText('Deleting')).toBeInTheDocument()
    expect(screen.getByText('Suspended')).toBeInTheDocument()
    expect(requests).toEqual([])
    // A task already being deleted cannot be deleted again.
    expect(screen.getByRole('button', { name: 'Delete task going' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Delete task gated' })).toBeEnabled()
  })

  it('navigates when any part of a row is clicked', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    render(<TaskList />)
    await user.click(await screen.findByText('Succeeded'))
    expect(navigateMock).toHaveBeenCalledWith({ to: '/tasks/$taskId', params: { taskId: 'del-task' } })
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

describe('TaskList paging and access', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('loads every page so a new task whose name sorts last still lists first', async () => {
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const url = new URL(request.url)
        if (url.searchParams.get('continue') === 'page-2') {
          return HttpResponse.json({
            items: [{ metadata: { name: 'zeta-task', namespace: 'default', uid: 'z', creationTimestamp: '2026-03-01T00:00:00Z' }, spec: { type: 'ai' }, status: { phase: 'Succeeded' } }],
            metadata: {},
          })
        }
        return HttpResponse.json({
          items: [{ metadata: { name: 'alpha-task', namespace: 'default', uid: 'a', creationTimestamp: '2026-01-01T00:00:00Z' }, spec: { type: 'container' }, status: { phase: 'Running' } }],
          metadata: { continue: 'page-2', remainingItemCount: 1 },
        })
      }),
    )
    render(<TaskList />)
    await screen.findByText('zeta-task')
    const names = screen.getAllByRole('row').slice(1).map((row) => row.querySelector('a')?.textContent)
    expect(names).toEqual(['zeta-task', 'alpha-task'])
    expect(screen.queryByRole('button', { name: /Show more/ })).not.toBeInTheDocument()
    expect(screen.queryByText(/More tasks exist/)).not.toBeInTheDocument()
  })

  it('reveals rows incrementally and filters across every loaded task', async () => {
    const base = Date.parse('2026-01-01T00:00:00Z')
    const items = Array.from({ length: 30 }, (_, i) => ({
      metadata: {
        name: `task-${String(i).padStart(2, '0')}`,
        namespace: 'default',
        uid: `uid-${i}`,
        creationTimestamp: new Date(base + i * 60_000).toISOString(),
      },
      spec: { type: 'container' },
      status: { phase: 'Succeeded' },
    }))
    server.use(http.get('/api/v1/tasks', () => HttpResponse.json({ items, metadata: {} })))
    const user = userEvent.setup()
    render(<TaskList />)
    await screen.findByText('task-29')
    expect(screen.getAllByRole('row')).toHaveLength(1 + 25)
    // The oldest task is loaded but not revealed yet; the filter still finds it.
    expect(screen.queryByText('task-00')).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('Filter tasks'), 'task-00')
    expect(screen.getByText('task-00')).toBeInTheDocument()
    expect(screen.getByText('1 of 30 loaded')).toBeInTheDocument()
    await user.clear(screen.getByLabelText('Filter tasks'))

    await user.click(screen.getByRole('button', { name: 'Show more (5 remaining)' }))
    expect(screen.getAllByRole('row')).toHaveLength(1 + 30)
    expect(screen.getByText('task-00')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Show more/ })).not.toBeInTheDocument()
  })

  it('says so when the list walk stopped at the page cap', async () => {
    let calls = 0
    server.use(
      http.get('/api/v1/tasks', () => {
        calls += 1
        return HttpResponse.json({
          items: [{ metadata: { name: `task-${calls}`, namespace: 'default', uid: `uid-${calls}` }, spec: { type: 'agent' }, status: { phase: 'Running' } }],
          metadata: { continue: `page-${calls}` },
        })
      }),
    )
    render(<TaskList />)
    expect(await screen.findByRole('status')).toHaveTextContent(
      `Only the first ${maxListWalkPages} tasks in resource-key order were loaded, so newer tasks may be missing. More tasks exist.`,
    )
    expect(screen.getByText(`${maxListWalkPages} loaded · more available`)).toBeInTheDocument()
  })

  it('truncates a long name at every width and keeps the full name in its title', async () => {
    const name = 'a-very-long-task-name-that-would-push-phase-and-delete-off-a-phone-screen-x'
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [{ metadata: { name, namespace: 'default', uid: 'long' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } }],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    const link = await screen.findByRole('link', { name })
    expect(link).toHaveAttribute('title', name)
    expect(link).toHaveClass('block', 'truncate')
    // No breakpoint lifts the cap: at 1024 and 1280 every column is shown
    // and a long name must still give way to them.
    const cell = link.closest('td')!
    expect(cell).toHaveClass('w-full', 'max-w-0')
    expect(cell.className).not.toMatch(/\b(sm|md|lg|xl):(w-auto|max-w-none)\b/)
    const phaseCell = cell.closest('tr')!.querySelectorAll('td')[2]
    expect(phaseCell.firstElementChild).toHaveClass('flex-wrap', 'lg:flex-nowrap')
    expect(screen.getByRole('columnheader', { name: 'Age' })).toHaveClass('hidden', 'sm:table-cell')
    expect(screen.getByRole('button', { name: `Delete task ${name}` }).closest('td')).not.toHaveClass('hidden')
  })

  it('lists loaded tasks newest first and keeps Name and Phase visible on narrow screens', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            { metadata: { name: 'aaa-oldest', namespace: 'default', uid: '1', creationTimestamp: '2026-01-01T00:00:00Z' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } },
            { metadata: { name: 'zzz-newest', namespace: 'default', uid: '2', creationTimestamp: '2026-03-01T00:00:00Z' }, spec: { type: 'container' }, status: { phase: 'Running' } },
            { metadata: { name: 'mmm-middle', namespace: 'default', uid: '3', creationTimestamp: '2026-02-01T00:00:00Z' }, spec: { type: 'ai' }, status: { phase: 'Failed' } },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    await screen.findByText('zzz-newest')
    const names = screen.getAllByRole('row').slice(1).map((row) => row.querySelector('a')?.textContent)
    expect(names).toEqual(['zzz-newest', 'mmm-middle', 'aaa-oldest'])
    expect(screen.getByRole('columnheader', { name: 'Namespace' })).toHaveClass('hidden', 'md:table-cell')
    expect(screen.getByRole('columnheader', { name: 'Type' })).toHaveClass('hidden', 'sm:table-cell')
    // The delete column is named for screen readers.
    expect(screen.getByRole('columnheader', { name: 'Actions' })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: 'Phase' })).not.toHaveClass('hidden')
  })

  it('filtering by "ai" does not match Container tasks', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            { metadata: { name: 'build', namespace: 'default', uid: '1' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } },
            { metadata: { name: 'summarize', namespace: 'default', uid: '2' }, spec: { type: 'ai' }, status: { phase: 'Succeeded' } },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    await screen.findByText('build')
    await user.type(screen.getByLabelText('Filter tasks'), 'ai')
    expect(screen.getByText('summarize')).toBeInTheDocument()
    expect(screen.queryByText('build')).not.toBeInTheDocument()
  })

  it('shows a not-authorized panel instead of the empty-state CTA on 403', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      ),
    )
    render(<TaskList />)
    await waitFor(() => expect(screen.getByText('Not authorized to view tasks')).toBeInTheDocument())
    expect(screen.queryByText(/No tasks found/)).not.toBeInTheDocument()
  })
})
