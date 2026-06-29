import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
const navigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, useNavigate: () => navigate }
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

  it('shows follow/refresh/open/fork/delete', () => {
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} />)
    expect(screen.getByRole('button', { name: /following/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /refresh/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /open/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /fork/i })).toBeInTheDocument()
  })

  it('requires confirmation before delete', async () => {
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} />)
    await userEvent.click(screen.getByRole('button', { name: /^delete/i }))
    expect(screen.getByRole('button', { name: /confirm delete/i })).toBeInTheDocument()
  })

  it('navigates to /tasks after confirming delete', async () => {
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} />)
    await userEvent.click(screen.getByRole('button', { name: /^delete/i }))
    await userEvent.click(screen.getByRole('button', { name: /confirm delete/i }))
    expect(navigate).toHaveBeenCalledWith({ to: '/tasks' })
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

  it('fork pins afterSeq and sends an Idempotency-Key header', async () => {
    let idem: string | null = 'missing'
    let body: { afterSeq?: number } = {}
    const { http, HttpResponse } = await import('msw')
    const { server } = await import('@/test/mocks/server')
    server.use(http.post('/api/v1/tasks/:id/fork', async ({ request }) => {
      idem = request.headers.get('idempotency-key')
      body = await request.json() as { afterSeq?: number }
      return HttpResponse.json({ namespace: 'default', sourceTaskName: 't1', newTaskName: 't1-fork', afterSeq: 0, forkContext: { sourceNamespace: 'default', sourceTask: 't1', afterSeq: 0, events: [], truncated: false } })
    }))
    render(<RuntimeControlBar task={task} following onToggleFollow={() => {}} latestSeq={42} />)
    await userEvent.click(screen.getByRole('button', { name: /fork/i }))
    await new Promise((r) => setTimeout(r, 50))
    expect(idem).toBeTruthy()
    expect(body.afterSeq).toBe(42)
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
