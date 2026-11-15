import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => <a href={params?.taskId ? to.replace('$taskId', params.taskId) : to} {...props}>{children}</a>,
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskSchedulePanel } from './task-schedule-panel'
import type { Task } from '@/schemas/task'

const parent: Task = {
  metadata: { name: 'nightly', namespace: 'default', uid: 'parent-uid' },
  spec: { type: 'container', schedule: '0 2 * * *', timeZone: 'Europe/Istanbul', concurrencyPolicy: 'Allow' },
  status: {
    phase: 'Scheduled',
    lastScheduleTime: new Date(Date.now() - 3600_000).toISOString(),
    nextScheduleTime: new Date(Date.now() + 7200_000).toISOString(),
  },
}

function run(name: string, ownerUID: string, minutesAgo: number, phase: string): Task {
  return {
    metadata: {
      name,
      namespace: 'default',
      uid: `uid-${name}`,
      creationTimestamp: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
      labels: { 'orka.ai/parent-task': 'nightly', 'orka.ai/scheduled-run': 'true' },
      ownerReferences: [{ apiVersion: 'core.orka.ai/v1alpha1', kind: 'Task', name: 'nightly', uid: ownerUID, controller: true }],
    },
    spec: { type: 'container' },
    status: { phase } as Task['status'],
  }
}

describe('TaskSchedulePanel', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 't' })
  })

  it('renders nothing for an unscheduled task', () => {
    const { container } = render(<TaskSchedulePanel task={{ ...parent, spec: { type: 'container' } }} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows the schedule, last and next run, and links the recent runs this Task owns', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/children', () =>
        HttpResponse.json({
          items: [
            run('nightly-older', 'parent-uid', 120, 'Failed'),
            run('nightly-newer', 'parent-uid', 10, 'Succeeded'),
            // Owned by an earlier Task with the same name: not this schedule's run.
            run('nightly-stale', 'old-parent-uid', 5, 'Succeeded'),
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskSchedulePanel task={parent} />)
    expect(screen.getByText('0 2 * * *')).toBeInTheDocument()
    expect(screen.getByText('Europe/Istanbul')).toBeInTheDocument()
    expect(screen.getByText(/Concurrency:/).parentElement).toHaveTextContent('Allow')
    expect(screen.getByText(/1h ago/)).toBeInTheDocument()
    expect(screen.getByText(/\(in 1h\)|\(in 2h\)/)).toBeInTheDocument()

    const newer = await screen.findByRole('link', { name: 'nightly-newer' })
    expect(newer).toHaveAttribute('href', '/tasks/nightly-newer')
    const links = screen.getAllByRole('link').map((link) => link.textContent)
    expect(links).toEqual(['nightly-newer', 'nightly-older'])
    expect(screen.queryByText('nightly-stale')).not.toBeInTheDocument()
  })

  it('marks a suspended schedule and shows no next run', () => {
    render(<TaskSchedulePanel task={{ ...parent, spec: { ...parent.spec, suspend: true } }} />)
    expect(screen.getByText('Suspended', { selector: '[data-slot="badge"]' })).toBeInTheDocument()
    expect(screen.getByText(/Next run:/).parentElement).toHaveTextContent('Next run: Suspended')
  })
})
