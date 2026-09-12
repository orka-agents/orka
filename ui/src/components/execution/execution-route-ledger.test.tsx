import { describe, expect, it } from 'vitest'
import { render, screen } from '@/test/test-utils'
import type { Session } from '@/schemas/session'
import type { Task } from '@/schemas/task'
import {
  SessionExecutionRouteLedger,
  TaskExecutionRouteLedger,
} from './execution-route-ledger'
import { abbreviateEvidence } from './execution-route-format'

const digest = (character: string) => `sha256:${character.repeat(64)}`

function agentTask(status: Task['status']): Task {
  return {
    metadata: { name: 'isolated-task', namespace: 'default', uid: 'task-uid' },
    spec: { type: 'agent', agentRef: { name: 'coder' }, prompt: 'run' },
    status,
  }
}

function harnessV1Binding(): NonNullable<NonNullable<Task['status']>['agentExecutionBinding']> {
  return {
    schemaVersion: 1,
    contractVersion: 'orka.harness.v1',
    backend: 'harness-wrapper',
    bindingDigest: digest('c'),
    task: { namespaceUID: 'namespace-uid', uid: 'task-uid', boundSpecGeneration: 2 },
    snapshot: { id: 'task-uid/snapshot', digest: digest('d'), schemaVersion: 1 },
    runtimeType: 'codex',
    boundAt: '2026-08-06T00:00:00Z',
  }
}

describe('TaskExecutionRouteLedger', () => {
  it('shows the immutable v2 route, snapshot, and static installation contract without full digests', () => {
    const { container } = render(
      <TaskExecutionRouteLedger
        task={agentTask({
          phase: 'Running',
          agentExecutionBinding: {
            schemaVersion: 1,
            contractVersion: 'orka.harness.v2',
            backend: 'runtime-pool',
            bindingDigest: digest('a'),
            task: { namespaceUID: 'namespace-uid', uid: 'task-uid', boundSpecGeneration: 2 },
            snapshot: { id: 'task-uid/snapshot', digest: digest('b'), schemaVersion: 1 },
            runtimeType: 'codex',
            boundAt: '2026-08-06T00:00:00Z',
          },
        })}
      />,
    )

    expect(screen.getByText('Route locked')).toBeInTheDocument()
    expect(screen.getByText('ACP v2 · runtime pool')).toBeInTheDocument()
    expect(screen.getByText('Static namespace route')).toBeInTheDocument()
    expect(screen.getByText('Contract orka.harness.v2 is fixed by the installation mode')).toBeInTheDocument()
    expect(screen.getAllByText(/sha256:aaaaaaaaaa…aaaaaaaa|sha256:bbbbbbbbbb…bbbbbbbb/)).toHaveLength(2)
    expect(screen.queryByText(digest('a'))).not.toBeInTheDocument()
    expect(container.innerHTML).not.toContain(digest('a'))
    expect(container.innerHTML).not.toContain(digest('b'))
  })

  it('requires reconciliation for a harness v1 unknown outcome', () => {
    render(
      <TaskExecutionRouteLedger
        task={agentTask({
          phase: 'Failed',
          agentExecutionBinding: harnessV1Binding(),
          harnessRuntime: {
            state: 'OutcomeUnknown',
            outcome: 'OutcomeUnknown',
            reason: 'WrapperRestarted',
            message: 'accepted turn could not be settled after restart',
          },
        })}
      />,
    )

    expect(screen.getByText('Outcome unknown')).toHaveClass('text-status-failed')
    expect(screen.getByText('Human reconciliation required')).toBeInTheDocument()
    expect(screen.getByText('WrapperRestarted')).toBeInTheDocument()
  })

})

describe('SessionExecutionRouteLedger', () => {
  it('keeps missing availability in an explicit warning state', () => {
    const session: Session = {
      name: 'initializing-session',
      namespace: 'default',
      executionControl: {
        sessionUID: 'session-uid-0000000000000001',
        generation: 1,
        lifecycle: 'Active',
      },
    }

    render(<SessionExecutionRouteLedger session={session} />)

    expect(screen.getByText('Availability unknown')).toHaveClass('text-status-pending')
    expect(screen.getByText('Lineage not yet established')).toBeInTheDocument()
    expect(screen.getByText('Availability not yet established')).toBeInTheDocument()
  })

  it('shows lineage evidence and a blocked continuation requiring adjudication', () => {
    const session: Session = {
      name: 'blocked-session',
      namespace: 'default',
      executionControl: {
        sessionUID: 'session-uid-0000000000000001',
        generation: 3,
        lifecycle: 'Finalizing',
        availability: 'ReconciliationBlocked',
        mutationLeaseGeneration: 8,
        blockedReason: 'publication outcome could not be independently verified',
        lineage: {
          namespaceUID: 'namespace-uid',
          sessionUID: 'session-uid-0000000000000001',
          contractVersion: 'orka.harness.v2',
          generation: 1,
          runtimeIdentity: 'opencode',
          configDigest: digest('c'),
          establishedAt: '2026-08-06T00:00:00Z',
        },
      },
    }

    render(<SessionExecutionRouteLedger session={session} />)

    expect(screen.getByText('Reconciliation required')).toBeInTheDocument()
    expect(screen.getByText('ACP v2 · lineage 1')).toBeInTheDocument()
    expect(screen.getByText('Continuation is blocked')).toBeInTheDocument()
    expect(screen.getByText(session.executionControl!.blockedReason!)).toBeInTheDocument()
  })
})

describe('abbreviateEvidence', () => {
  it('keeps small values and abbreviates sha256 evidence', () => {
    expect(abbreviateEvidence('small')).toBe('small')
    expect(abbreviateEvidence(digest('f'))).toBe('sha256:ffffffffff…ffffffff')
  })
})
