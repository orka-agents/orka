import { describe, expect, it } from 'vitest'
import { render, screen } from '@/test/test-utils'
import type { Session } from '@/schemas/session'
import { SessionExecutionRouteLedger } from './execution-route-ledger'
import { abbreviateEvidence } from './execution-route-format'

const digest = (character: string) => `sha256:${character.repeat(64)}`

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
    expect(screen.getByText('Lineage 1')).toBeInTheDocument()
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
