import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
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

  it('count chips use token tint/text classes, not pastels', async () => {
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
    expect(chip).toHaveClass('bg-status-running-bg')
    expect(chip.className).not.toMatch(/bg-(yellow|blue|green|red)-/)
  })
})
