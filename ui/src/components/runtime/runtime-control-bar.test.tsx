import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
const navigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    useNavigate: () => navigate,
    Link: ({ children, to, params, ...props }: any) => <a href={params?.taskId ? to.replace('$taskId', params.taskId) : to} {...props}>{children}</a>,
  }
})
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { RuntimeControlBar } from './runtime-control-bar'
import type { Task } from '@/schemas/task'

const task: Task = { metadata: { name: 't1', namespace: 'default', uid: 'u1' }, spec: { type: 'agent' }, status: { phase: 'Running' } }

describe('RuntimeControlBar', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 't' })
    navigate.mockClear()
  })

  it('shows follow/refresh/fork and leaves delete to the task header', () => {
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} />)
    expect(screen.getByRole('button', { name: /following/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /refresh/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /fork/i })).toBeInTheDocument()
    // The header Delete is visible on every tab; Open only reloaded this page.
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /open/i })).not.toBeInTheDocument()
  })

  it('refresh invalidates the task-specific query keys, not just the list', async () => {
    const { QueryClient, QueryClientProvider } = await import('@tanstack/react-query')
    const { render: rawRender } = await import('@testing-library/react')
    const qc = new QueryClient()
    const spy = vi.spyOn(qc, 'invalidateQueries')
    rawRender(
      <QueryClientProvider client={qc}>
        <RuntimeControlBar task={task} following onToggleFollow={() => {}} />
      </QueryClientProvider>,
    )
    await userEvent.click(screen.getByRole('button', { name: /refresh/i }))
    const keys = spy.mock.calls.map((c) => (c[0] as { queryKey: string[] }).queryKey[0])
    expect(keys).toContain('task')
    expect(keys).toContain('taskTrace')
    expect(keys).toContain('taskArtifacts')
  })

  it('fork opens the confirm dialog at the latest seq and only forks on confirm', async () => {
    let idem: string | null = 'missing'
    let body: { afterSeq?: number; newTaskName?: string } = {}
    let posts = 0
    const { http, HttpResponse } = await import('msw')
    const { server } = await import('@/test/mocks/server')
    server.use(http.post('/api/v1/tasks/:id/fork', async ({ request }) => {
      posts += 1
      idem = request.headers.get('idempotency-key')
      body = await request.json() as { afterSeq?: number; newTaskName?: string }
      return HttpResponse.json({ namespace: 'default', sourceTaskName: 't1', newTaskName: 't1-branch', afterSeq: 42, forkContext: { sourceNamespace: 'default', sourceTask: 't1', afterSeq: 42, events: [], truncated: false } }, { status: 201 })
    }))
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} latestSeq={42} />)
    await userEvent.click(screen.getByRole('button', { name: /fork/i }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent('#42')
    expect(posts).toBe(0)

    await userEvent.type(screen.getByLabelText(/New task name/), 't1-branch')
    await userEvent.click(screen.getByRole('button', { name: /create fork/i }))
    expect(await screen.findByRole('link', { name: /t1-branch/ })).toBeInTheDocument()
    expect(posts).toBe(1)
    expect(idem).toBeTruthy()
    expect(body).toMatchObject({ afterSeq: 42, newTaskName: 't1-branch' })
    // The new task is offered as a link, not opened behind the user's back.
    expect(navigate).not.toHaveBeenCalled()
  })

  it('shows the server reason when the fork fails', async () => {
    const { http, HttpResponse } = await import('msw')
    const { server } = await import('@/test/mocks/server')
    server.use(http.post('/api/v1/tasks/:id/fork', () =>
      HttpResponse.json({ error: { code: 409, message: 'forked task already exists' } }, { status: 409 }),
    ))
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} latestSeq={3} />)
    await userEvent.click(screen.getByRole('button', { name: /fork/i }))
    await userEvent.click(await screen.findByRole('button', { name: /create fork/i }))
    expect(await screen.findByText('forked task already exists')).toBeInTheDocument()
  })

  it('hides Fork when execution-event storage is unsupported', () => {
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} forkSupported={false} />)
    expect(screen.queryByRole('button', { name: /fork/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /refresh/i })).toBeInTheDocument()
  })

  it('disables Fork until a sequence is known', () => {
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} />)
    expect(screen.getByRole('button', { name: /fork/i })).toBeDisabled()
  })

  it('toggles follow', async () => {
    const toggle = vi.fn()
    render(<RuntimeControlBar task={task} following={false} onToggleFollow={toggle} />)
    await userEvent.click(screen.getByRole('button', { name: /paused/i }))
    expect(toggle).toHaveBeenCalled()
  })
})
