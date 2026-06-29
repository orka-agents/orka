import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { TaskFlowPanel } from './task-flow-panel'
import type { Task } from '@/schemas/task'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, useNavigate: () => vi.fn() }
})

const task = (name: string, phase = 'Running'): Task => ({
  metadata: { name, namespace: 'default', uid: name }, spec: { type: 'agent' }, status: { phase: phase as Task['status']['phase'] },
})

describe('TaskFlowPanel', () => {
  it('renders single task graph', () => {
    render(<TaskFlowPanel task={task('root')} />)
    expect(screen.getByRole('tree', { name: /execution graph/i })).toBeInTheDocument()
  })

  it('renders fallback list of running tasks when no root', () => {
    render(<TaskFlowPanel tasks={[task('a'), task('b'), task('s', 'Succeeded')]} />)
    expect(screen.getByText('a')).toBeInTheDocument()
    expect(screen.getByText('b')).toBeInTheDocument()
  })

  it('empty state when nothing selectable', () => {
    render(<TaskFlowPanel tasks={[]} />)
    expect(screen.getByText('No active flow')).toBeInTheDocument()
  })
})
