import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { LiveStatePanel } from './live-state-panel'
import type { Task } from '@/schemas/task'

const fullTask: Task = {
  metadata: {
    name: 'parent',
    namespace: 'default',
    labels: { 'app.kubernetes.io/name': 'orka' },
    annotations: { 'orka.ai/note': 'visible-note', 'orka.ai/api-token': 'sk-should-hide' },
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

describe('LiveStatePanel', () => {
  it('renders execution workspace pool and visible metadata, hides secret-looking keys', () => {
    render(<LiveStatePanel task={fullTask} />)
    expect(screen.getByText('pool-blue')).toBeInTheDocument()
    expect(screen.getByText('2.5')).toBeInTheDocument()
    expect(screen.getByText('1.2s')).toBeInTheDocument()
    expect(screen.getByText('session-7')).toBeInTheDocument()
    expect(screen.getByText('child-a')).toBeInTheDocument()
    expect(screen.getByText('visible-note')).toBeInTheDocument()
    expect(screen.queryByText('sk-should-hide')).not.toBeInTheDocument()
    expect(screen.queryByText('orka.ai/api-token')).not.toBeInTheDocument()
    expect(screen.getByText('read-only')).toBeInTheDocument()
  })

  it('treats legacy result references as available', () => {
    render(<LiveStatePanel task={{
      ...minimalTask,
      status: { phase: 'Succeeded', resultRef: { configMapName: 'legacy-result', key: 'out' } },
    }} />)
    expect(screen.getByText('Available')).toBeInTheDocument()
    expect(screen.getByText('Yes')).toBeInTheDocument()
    expect(screen.getByText('legacy-result')).toBeInTheDocument()
  })

  it('omits empty sections for a minimal task', () => {
    render(<LiveStatePanel task={minimalTask} />)
    expect(screen.getByText('Status')).toBeInTheDocument()
    expect(screen.queryByText('Conditions')).not.toBeInTheDocument()
    expect(screen.queryByText('Child tasks')).not.toBeInTheDocument()
    expect(screen.queryByText('Execution workspace')).not.toBeInTheDocument()
    expect(screen.queryByText('Session')).not.toBeInTheDocument()
    expect(screen.queryByText('Result')).not.toBeInTheDocument()
    expect(screen.queryByText('Metadata')).not.toBeInTheDocument()
  })
})
