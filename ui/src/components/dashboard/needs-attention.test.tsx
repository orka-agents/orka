import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
  }
})

import { NeedsAttention } from './needs-attention'
import type { Task } from '@/schemas/task'

function task(name: string, waiting: boolean): Task {
  return {
    metadata: { name, namespace: 'default', uid: `uid-${name}` },
    spec: { type: 'agent', agentRef: { name: 'coder' } },
    status: {
      phase: 'Running',
      conditions: waiting
        ? [{ type: 'WaitingForApproval', status: 'True' }]
        : [{ type: 'Complete', status: 'False' }],
    },
  } as Task
}

describe('NeedsAttention', () => {
  it('renders nothing when no task waits for approval', () => {
    const { container } = render(<NeedsAttention tasks={[task('a', false)]} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('lists tasks parked on WaitingForApproval with a count', () => {
    render(<NeedsAttention tasks={[task('a', true), task('b', true), task('c', false)]} />)
    expect(screen.getByText('Waiting for approval')).toBeInTheDocument()
    expect(screen.getByText('2')).toBeInTheDocument()
    expect(screen.getByText('a')).toBeInTheDocument()
    expect(screen.getByText('b')).toBeInTheDocument()
    expect(screen.queryByText('c')).not.toBeInTheDocument()
  })

  it('caps the list at six and summarizes the rest', () => {
    const many = Array.from({ length: 8 }, (_, i) => task(`t${i}`, true))
    render(<NeedsAttention tasks={many} />)
    expect(screen.getByText(/and 2 more/i)).toBeInTheDocument()
  })
})
