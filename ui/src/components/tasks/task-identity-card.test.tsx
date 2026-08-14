import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'

import { TaskIdentityCard } from './task-identity-card'
import type { Task } from '@/schemas/task'

const base: Task = {
  metadata: { name: 't1', namespace: 'default' },
  spec: { type: 'ai' },
} as Task

describe('TaskIdentityCard', () => {
  it('renders nothing without stamped identity', () => {
    const { container } = render(<TaskIdentityCard task={base} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows requestedBy fields', () => {
    const task = {
      ...base,
      spec: {
        ...base.spec,
        requestedBy: {
          username: 'system:serviceaccount:default:ci',
          subject: 'sub-1',
          groups: ['system:authenticated'],
        },
      },
    } as Task
    render(<TaskIdentityCard task={task} />)
    expect(screen.getByText('Requested by')).toBeInTheDocument()
    expect(screen.getByText('system:serviceaccount:default:ci')).toBeInTheDocument()
    expect(screen.getByText('system:authenticated')).toBeInTheDocument()
  })

  it('shows transaction metadata when present', () => {
    const task = {
      ...base,
      spec: {
        ...base.spec,
        transaction: {
          profile: 'transaction-token',
          id: 'tx-99',
          scopes: ['orka:tasks:create'],
          requestingWorkload: 'ci-runner',
        },
      },
    } as Task
    render(<TaskIdentityCard task={task} />)
    expect(screen.getByText('Transaction')).toBeInTheDocument()
    expect(screen.getByText('tx-99')).toBeInTheDocument()
    expect(screen.getByText('orka:tasks:create')).toBeInTheDocument()
    expect(screen.getByText('ci-runner')).toBeInTheDocument()
  })
})
