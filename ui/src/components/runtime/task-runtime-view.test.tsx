import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to }: any) => <a href={to}>{children}</a>, useNavigate: () => vi.fn() }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskRuntimeView } from './task-runtime-view'
import type { Task } from '@/schemas/task'

const task = (phase = 'Running'): Task => ({
  metadata: { name: 'rt', namespace: 'default', uid: 'u' }, spec: { type: 'agent', agentRef: { name: 'a' } }, status: { phase: phase as Task['status']['phase'] },
})

describe('TaskRuntimeView', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 't' })
  })

  it('renders runtime panels for a running task', async () => {
    render(<TaskRuntimeView task={task()} events={[]} />)
    await waitFor(() => expect(screen.getByText('Task flow')).toBeInTheDocument())
    expect(screen.getByText('Derived checks')).toBeInTheDocument()
    expect(screen.getByText('Live state')).toBeInTheDocument()
  })

  it('renders for a succeeded task without crashing', async () => {
    render(<TaskRuntimeView task={task('Succeeded')} events={[]} />)
    await waitFor(() => expect(screen.getByText('Task flow')).toBeInTheDocument())
  })
})
