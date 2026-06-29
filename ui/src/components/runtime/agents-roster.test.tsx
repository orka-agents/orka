import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { AgentsRoster } from './agents-roster'
import type { Task } from '@/schemas/task'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a> }
})

function task(name: string, agent?: string): Task {
  return {
    metadata: { name, namespace: 'default', uid: name },
    spec: { type: 'agent', ...(agent ? { agentRef: { name: agent } } : {}) },
    status: { phase: 'Running' },
  }
}

describe('AgentsRoster', () => {
  it('shows empty state with no tasks', () => {
    render(<AgentsRoster tasks={[]} />)
    expect(screen.getByText('No agents active')).toBeInTheDocument()
  })

  it('groups by agent and shows an Unassigned bucket', () => {
    render(<AgentsRoster tasks={[task('a', 'alpha'), task('b', 'alpha'), task('c')]} />)
    expect(screen.getByText('alpha')).toBeInTheDocument()
    expect(screen.getByText('Unassigned')).toBeInTheDocument()
    expect(screen.getByText('a')).toBeInTheDocument()
    expect(screen.getByText('c')).toBeInTheDocument()
  })

  it('highlights the active task row', () => {
    render(<AgentsRoster tasks={[task('a', 'alpha')]} activeTaskName="a" />)
    const link = screen.getByText('a').closest('a')
    expect(link?.className).toContain('border-l-live')
  })
})
