import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { TaskStatusDetails } from './task-status-details'
import type { Task } from '@/schemas/task'

const fullTask: Task = {
  metadata: {
    name: 'parent',
    namespace: 'default',
    labels: { 'app.kubernetes.io/name': 'orka' },
    annotations: { 'orka.ai/note': 'visible-note', 'orka.ai/api-token': 'sk-should-hide', 'orka.ai/description': 'sk-should-hide', 'example.com/callback': 'https://cb.example.com/done?sig=abc' },
  },
  spec: { type: 'agent', sessionRef: { name: 'session-7' } },
  status: {
    phase: 'Running',
    conditions: [{ type: 'Ready', status: 'True', message: 'workspace ready' }],
    childTasks: [{ name: 'child-a', agent: 'alpha', phase: 'Succeeded', result: 'ok' }],
    executionWorkspace: {
      provider: 'substrate',
      phase: 'Active',
      reused: true,
      placement: { workerNamespace: 'workers', workerPool: 'pool-blue', workerPodName: 'pod-1' },
      density: { workerCount: 2, actorCount: 5, runningActorCount: 3, suspendedActorCount: 2, actorsPerWorker: '2.5' },
      resumeLatency: '1.2s',
      message: 'placed',
    },
    resultRef: { available: true, configMapName: 'parent-result', key: 'out' },
  },
}

const minimalTask: Task = {
  metadata: { name: 'mini', namespace: 'default' },
  spec: { type: 'container' },
  status: { phase: 'Pending' },
}

describe('TaskStatusDetails', () => {
  it('renders execution workspace pool and visible metadata, hides secret-looking keys', () => {
    render(<TaskStatusDetails task={fullTask} />)
    expect(screen.getByText('pool-blue')).toBeInTheDocument()
    expect(screen.getByText('2.5')).toBeInTheDocument()
    expect(screen.getByText('1.2s')).toBeInTheDocument()
    expect(screen.getByText('session-7')).toBeInTheDocument()
    expect(screen.getByText('visible-note')).toBeInTheDocument()
    expect(screen.queryByText('sk-should-hide')).not.toBeInTheDocument()
    expect(screen.queryByText('example.com/callback')).not.toBeInTheDocument()
    expect(screen.queryByText('orka.ai/api-token')).not.toBeInTheDocument()
    expect(screen.queryByText('orka.ai/description')).not.toBeInTheDocument()
  })

  it('wraps a long unbroken condition message and keeps the status badge beside the type', () => {
    const approvalID = 'a'.repeat(64)
    render(<TaskStatusDetails task={{
      ...minimalTask,
      status: {
        phase: 'Running',
        conditions: [{ type: 'WaitingForApproval', status: 'True', message: `waiting for approval ${approvalID}` }],
      },
    }} />)
    expect(screen.getByText(`waiting for approval ${approvalID}`)).toHaveClass('wrap-anywhere')
    const badge = screen.getByText('True')
    expect(badge).toHaveClass('shrink-0')
    const type = screen.getByText('WaitingForApproval')
    expect(type).toHaveClass('min-w-0', 'wrap-anywhere')
    expect(type.parentElement).toContainElement(badge)
  })

  it('treats legacy result references as available', () => {
    render(<TaskStatusDetails task={{
      ...minimalTask,
      status: { phase: 'Succeeded', resultRef: { configMapName: 'legacy-result', key: 'out' } },
    }} />)
    expect(screen.getByText('Available')).toBeInTheDocument()
    expect(screen.getByText('Yes')).toBeInTheDocument()
    expect(screen.getByText('legacy-result')).toBeInTheDocument()
  })

  it('leaves out its sections, and itself, when a task has none of them', () => {
    const { container } = render(<TaskStatusDetails task={minimalTask} />)
    expect(container).toBeEmptyDOMElement()

    render(<TaskStatusDetails task={{ ...minimalTask, spec: { type: 'agent', sessionRef: { name: 'session-7' } } }} />)
    expect(screen.getByText('Session')).toBeInTheDocument()
    expect(screen.queryByText('Conditions')).not.toBeInTheDocument()
    expect(screen.queryByText('Execution workspace')).not.toBeInTheDocument()
    expect(screen.queryByText('Result')).not.toBeInTheDocument()
    expect(screen.queryByText('Labels and annotations')).not.toBeInTheDocument()
  })
})
