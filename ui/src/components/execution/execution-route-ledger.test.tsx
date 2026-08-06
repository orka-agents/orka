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
    metadata: { name: 'coexistence-task', namespace: 'default', uid: 'task-uid' },
    spec: { type: 'agent', agentRef: { name: 'coder' }, prompt: 'run' },
    status,
  }
}

describe('TaskExecutionRouteLedger', () => {
  it('shows the immutable v2 route, snapshot, and admission revision without full digests', () => {
    const { container } = render(
      <TaskExecutionRouteLedger
        task={agentTask({
          phase: 'Running',
          agentExecutionBinding: {
            schemaVersion: 1,
            mode: 'execute',
            contractVersion: 'orka.harness.v2',
            backend: 'runtime-pool',
            provenance: 'newly-bound',
            bindingDigest: digest('a'),
            task: { namespaceUID: 'namespace-uid', uid: 'task-uid', boundSpecGeneration: 2 },
            backendControl: {
              name: 'cluster', uid: 'control-uid', generation: 4, modeRevision: 12, admittedMode: 'enabled',
            },
            snapshot: { id: 'task-uid/snapshot', digest: digest('b'), schemaVersion: 1 },
            runtimeType: 'codex',
            boundAt: '2026-08-06T00:00:00Z',
          },
        })}
      />,
    )

    expect(screen.getByText('Route locked')).toBeInTheDocument()
    expect(screen.getByText('ACP v2 · runtime pool')).toBeInTheDocument()
    expect(screen.getByText('enabled · revision 12')).toBeInTheDocument()
    expect(screen.getAllByText(/sha256:aaaaaaaaaa…aaaaaaaa|sha256:bbbbbbbbbb…bbbbbbbb/)).toHaveLength(2)
    expect(screen.queryByText(digest('a'))).not.toBeInTheDocument()
    expect(container.innerHTML).not.toContain(digest('a'))
    expect(container.innerHTML).not.toContain(digest('b'))
  })

  it('makes immutable quarantine evidence and required reconciliation explicit', () => {
    render(
      <TaskExecutionRouteLedger
        task={agentTask({
          phase: 'Finalizing',
          agentExecutionQuarantine: {
            schemaVersion: 1,
            reason: 'MixedV1V2Evidence',
            migrationInventoryID: 'migration-2026-08-06',
            v1EvidenceDigest: digest('1'),
            v2EvidenceDigest: digest('2'),
            recordedAt: '2026-08-06T00:00:00Z',
          },
        })}
      />,
    )

    expect(screen.getByText('Reconciliation required')).toBeInTheDocument()
    expect(screen.getByText('Execution held in quarantine')).toBeInTheDocument()
    expect(screen.getByText('MixedV1V2Evidence')).toBeInTheDocument()
    expect(screen.getByText('v1')).toBeInTheDocument()
    expect(screen.getByText('v2')).toBeInTheDocument()
    expect(screen.getByText('Operator decision required')).toBeInTheDocument()
  })

  it('renders an immutable no-execution disposition as common cleanup only', () => {
    render(
      <TaskExecutionRouteLedger
        task={agentTask({
          phase: 'Finalizing',
          agentExecutionNoExecution: {
            schemaVersion: 1,
            state: 'UnboundNoExecution',
            migrationInventoryID: 'migration-2026-08-06',
            evidenceDigest: digest('0'),
            recordedAt: '2026-08-06T00:00:00Z',
          },
        })}
      />,
    )

    expect(screen.getByText('No executor state')).toBeInTheDocument()
    expect(screen.getByText('Common cleanup only')).toBeInTheDocument()
    expect(screen.getByText('No-route proof recorded')).toBeInTheDocument()
    expect(screen.getByText('No operator decision required')).toBeInTheDocument()
  })
})

describe('SessionExecutionRouteLedger', () => {
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
          provenance: 'first-use',
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
