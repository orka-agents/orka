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
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('fills in each card as its own list arrives', () => {
    const { container } = render(
      <StatsCards
        tasks={[makeTask('t1', 'Running')]}
        agentCount={2}
        loading={{ tasks: false, sessions: true, agents: false, tools: true }}
      />,
    )
    expect(screen.getByText('Total Tasks')).toBeInTheDocument()
    expect(screen.getByText('2')).toBeInTheDocument()
    const card = (title: string) => screen.getByText(title).closest('[data-slot="card"]') as HTMLElement
    expect(card('Sessions').querySelector('[data-slot="skeleton"]')).not.toBeNull()
    expect(card('Tools').querySelector('[data-slot="skeleton"]')).not.toBeNull()
    expect(card('Agents').querySelector('[data-slot="skeleton"]')).toBeNull()
    expect(card('Total Tasks').querySelector('[data-slot="skeleton"]')).toBeNull()
    expect(container.querySelectorAll('[data-slot="skeleton"]')).toHaveLength(2)
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
    expect(screen.getByText('4')).toBeInTheDocument()
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('Failed')).toBeInTheDocument()
    expect(screen.getByText('5')).toBeInTheDocument()
    expect(screen.getByText('3')).toBeInTheDocument()
    expect(screen.getByText('7')).toBeInTheDocument()
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

  it('shows a not-authorized message on the Sessions card instead of 0', () => {
    render(<StatsCards tasks={[]} sessionsProblem={{ forbidden: true, message: "not authorized" }} agentCount={1} toolCount={2} />)
    expect(screen.getByText('Not authorized')).toBeInTheDocument()
    expect(screen.getByText(/read permission \(not authorized\)/)).toBeInTheDocument()
    expect(screen.getAllByText('0').length).toBe(4)
  })

  it('zero counts when no data', () => {
    render(<StatsCards />)
    expect(screen.getByText('Total Tasks')).toBeInTheDocument()
    const zeros = screen.getAllByText('0')
    expect(zeros.length).toBe(7)
  })

  it('shows a success-rate indicator derived from finished tasks', () => {
    const tasks = [
      makeTask('t1', 'Succeeded'),
      makeTask('t2', 'Succeeded'),
      makeTask('t3', 'Succeeded'),
      makeTask('t4', 'Failed'),
    ]
    render(<StatsCards tasks={tasks} />)
    expect(screen.getByText('75% success rate')).toBeInTheDocument()
  })

  it('omits the success rate when nothing has finished', () => {
    render(<StatsCards tasks={[makeTask('t1', 'Running'), makeTask('t2', 'Pending')]} />)
    expect(screen.queryByText(/success rate/)).not.toBeInTheDocument()
  })

  it('labels counts and rates as partial when more task pages exist', () => {
    render(<StatsCards tasks={[makeTask('t1', 'Succeeded')]} tasksTruncated />)
    expect(screen.getByText('Tasks loaded')).toBeInTheDocument()
    expect(screen.queryByText('Total Tasks')).not.toBeInTheDocument()
    expect(screen.getByText('100% of loaded finished tasks')).toBeInTheDocument()
  })

  it('labels the session count as loaded when more session pages exist', () => {
    render(<StatsCards tasks={[]} sessionCount={20} sessionsTruncated />)
    expect(screen.getByText('Sessions loaded')).toBeInTheDocument()
    expect(screen.queryByText('Sessions')).not.toBeInTheDocument()
  })

  it('renders trend sparklines when there is more than one task', () => {
    const tasks = [
      { metadata: { name: 'a', namespace: 'default', uid: 'a', creationTimestamp: '2026-01-01T00:00:00Z' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } },
      { metadata: { name: 'b', namespace: 'default', uid: 'b', creationTimestamp: '2026-01-02T00:00:00Z' }, spec: { type: 'container' }, status: { phase: 'Running' } },
    ] as Task[]
    render(<StatsCards tasks={tasks} />)
    expect(screen.getAllByRole('img', { name: /trend/i }).length).toBeGreaterThan(0)
  })

  it('plots a per-status trend, not the aggregate, on each status card', () => {
    const tasks = [
      { metadata: { name: 'a', namespace: 'default', uid: 'a', creationTimestamp: '2026-01-01T00:00:00Z' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } },
      { metadata: { name: 'b', namespace: 'default', uid: 'b', creationTimestamp: '2026-01-02T00:00:00Z' }, spec: { type: 'container' }, status: { phase: 'Succeeded' } },
    ] as Task[]
    render(<StatsCards tasks={tasks} />)
    const failed = screen.getByRole('img', { name: /failed trend/i })
    const succeeded = screen.getByRole('img', { name: /succeeded trend/i })
    const failedYs = new Set(
      (failed.querySelector('polyline')?.getAttribute('points') ?? '')
        .trim()
        .split(' ')
        .map((p) => p.split(',')[1]),
    )
    expect(failedYs.size).toBe(1)
    const succeededYs = new Set(
      (succeeded.querySelector('polyline')?.getAttribute('points') ?? '')
        .trim()
        .split(' ')
        .map((p) => p.split(',')[1]),
    )
    expect(succeededYs.size).toBeGreaterThan(1)
  })
})
