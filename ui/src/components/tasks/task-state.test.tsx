import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { isScheduleSuspended, isTaskDeleting, isWaitingForApproval, podWaitingMessage } from './task-state'
import { TaskStateBadges } from './task-state-badges'
import type { Task } from '@/schemas/task'

function task(o: { metadata?: Partial<Task['metadata']>; spec?: Partial<Task['spec']>; status?: Task['status'] } = {}): Task {
  return {
    metadata: { name: 't', namespace: 'default', ...o.metadata },
    spec: { type: 'agent', ...o.spec },
    status: o.status ?? { phase: 'Running' },
  }
}

describe('task state flags', () => {
  it('detects a WaitingForApproval condition', () => {
    expect(isWaitingForApproval(task({ status: { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True' }] } }))).toBe(true)
    expect(isWaitingForApproval(task({ status: { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'False' }] } }))).toBe(false)
  })

  it('detects an autonomous task parked on an approval by its status message', () => {
    expect(isWaitingForApproval(task({ status: { phase: 'Running', message: 'waiting for approval ap-1 for bash at iteration 2' } }))).toBe(true)
    expect(isWaitingForApproval(task({ status: { phase: 'Running', message: 'running tool' } }))).toBe(false)
  })

  it('never reports a terminal task as waiting', () => {
    expect(isWaitingForApproval(task({ status: { phase: 'Failed', message: 'waiting for approval ap-1 for bash at iteration 2' } }))).toBe(false)
  })

  it('reports a Running task whose Pod is stuck waiting, and nothing once it is terminal', () => {
    const message = 'pod waiting: ErrImagePull - pull access denied'
    expect(podWaitingMessage(task({ status: { phase: 'Running', message } }))).toBe(message)
    expect(podWaitingMessage(task({ status: { phase: 'Running', message: 'Task started with Job j-1' } }))).toBeUndefined()
    expect(podWaitingMessage(task({ status: { phase: 'Failed', message } }))).toBeUndefined()
  })

  it('detects deleting and suspended schedules', () => {
    expect(isTaskDeleting(task({ metadata: { deletionTimestamp: '2026-10-14T00:00:00Z' } }))).toBe(true)
    expect(isTaskDeleting(task())).toBe(false)
    expect(isScheduleSuspended(task({ spec: { schedule: '0 * * * *', suspend: true } }))).toBe(true)
    expect(isScheduleSuspended(task({ spec: { schedule: '0 * * * *', suspend: false } }))).toBe(false)
    expect(isScheduleSuspended(task({ spec: { suspend: true } }))).toBe(false)
  })

  it('renders a badge per active flag and nothing otherwise', () => {
    const { container, rerender } = render(<TaskStateBadges task={task()} />)
    expect(container).toBeEmptyDOMElement()
    rerender(
      <TaskStateBadges
        task={task({
          metadata: { deletionTimestamp: '2026-10-14T00:00:00Z' },
          spec: { schedule: '*/5 * * * *', suspend: true },
          status: { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True' }] },
        })}
      />,
    )
    expect(screen.getByText('Deleting')).toBeInTheDocument()
    expect(screen.getByText('Waiting for approval')).toBeInTheDocument()
    expect(screen.getByText('Suspended')).toBeInTheDocument()
  })
})
