import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { StatsCards } from './stats-cards'
import type { Task } from '@/schemas/task'

function makeTask(name: string, phase: string): Task {
  return {
    metadata: { name, namespace: 'default', uid: `uid-${name}` },
    spec: { type: 'container' },
    status: { phase: phase as any },
  }
}

describe('StatsCards', () => {
  it('loading state shows skeletons', () => {
    const { container } = render(<StatsCards isLoading />)
    // 4 skeleton cards rendered in loading state
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('renders correct counts for each task phase', () => {
    const tasks = [
      makeTask('t1', 'Running'),
      makeTask('t2', 'Running'),
      makeTask('t3', 'Succeeded'),
      makeTask('t4', 'Failed'),
    ]
    render(<StatsCards tasks={tasks} sessionCount={5} agentCount={3} toolCount={7} />)
    expect(screen.getByText('Total Tasks')).toBeInTheDocument()
    expect(screen.getByText('4')).toBeInTheDocument() // total
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('Failed')).toBeInTheDocument()
    expect(screen.getByText('5')).toBeInTheDocument() // sessions
    expect(screen.getByText('3')).toBeInTheDocument() // agents
    expect(screen.getByText('7')).toBeInTheDocument() // tools
  })

  it('renders sessions, agents, tools counts', () => {
    render(<StatsCards tasks={[]} sessionCount={10} agentCount={7} toolCount={4} />)
    expect(screen.getByText('Sessions')).toBeInTheDocument()
    expect(screen.getByText('10')).toBeInTheDocument()
    expect(screen.getByText('Agents')).toBeInTheDocument()
    expect(screen.getByText('7')).toBeInTheDocument()
    expect(screen.getByText('Tools')).toBeInTheDocument()
    expect(screen.getByText('4')).toBeInTheDocument()
  })

  it('zero counts when no data', () => {
    render(<StatsCards />)
    expect(screen.getByText('Total Tasks')).toBeInTheDocument()
    // All counts should show 0
    const zeros = screen.getAllByText('0')
    expect(zeros.length).toBe(7) // total, running, succeeded, failed, sessions, agents, tools
  })
})
