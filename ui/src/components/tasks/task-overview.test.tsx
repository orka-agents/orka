import { describe, it, expect, beforeEach, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to }: any) => <a href={to}>{children}</a>, useNavigate: () => vi.fn() }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskOverview } from './task-overview'
import type { Task } from '@/schemas/task'

const task = (phase = 'Running', status: Partial<NonNullable<Task['status']>> = {}): Task => ({
  metadata: { name: 'rt', namespace: 'default', uid: 'u' },
  spec: { type: 'agent', agentRef: { name: 'a' } },
  status: { phase: phase as NonNullable<Task['status']>['phase'], ...status },
})

describe('TaskOverview', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 't' })
  })

  it('shows progress and derived checks, and child tasks only when there are some', async () => {
    render(<TaskOverview task={task()} events={[]} />)
    await waitFor(() => expect(screen.getByText('Derived checks')).toBeInTheDocument())
    expect(screen.getByText('Execution')).toBeInTheDocument()
    expect(screen.queryByText('Child tasks')).not.toBeInTheDocument()
  })

  it('draws child tasks as a graph', async () => {
    render(<TaskOverview task={task('Running', { childTasks: [{ name: 'child-a', agent: 'alpha', phase: 'Succeeded' }] })} events={[]} />)
    expect(await screen.findByText('Child tasks')).toBeInTheDocument()
    expect(screen.getByRole('tree', { name: /execution graph/i })).toHaveTextContent('child-a')
  })

  it('points at waiting approvals', async () => {
    const onReview = vi.fn()
    render(<TaskOverview task={task()} events={[]} pendingApprovals={2} onReviewApprovals={onReview} />)
    expect(await screen.findByText(/2 approvals are waiting for a decision/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Review' }))
    expect(onReview).toHaveBeenCalled()
  })

  it('says when execution events failed to load or are turned off', async () => {
    const { unmount } = render(<TaskOverview task={task()} events={[]} eventsStatus="error" />)
    expect(await screen.findByRole('alert')).toHaveTextContent('Unable to load execution events')
    unmount()
    render(<TaskOverview task={task('Succeeded')} events={[]} eventsStatus="unsupported" />)
    expect(await screen.findByText(/Execution events are not enabled/)).toBeInTheDocument()
  })
})
