import { describe, it, expect } from 'vitest'
import { activeParkedApprovalId } from './parked-approval'

describe('activeParkedApprovalId', () => {
  it('derives the parked approval only from an active approval wait', () => {
    const task = (wait: Record<string, unknown> | undefined, deletionTimestamp?: string) => ({
      metadata: { name: 'tk', deletionTimestamp },
      status: { phase: 'Succeeded' as const, wait },
    })
    expect(activeParkedApprovalId(task({ kind: 'Approval', state: 'Waiting', references: ['ap'] }))).toBe('ap')
    expect(activeParkedApprovalId(task({ kind: 'Approval', state: 'Requested', references: ['ap'] }))).toBe('ap')
    expect(activeParkedApprovalId(task({ kind: 'Approval', state: 'Resuming', references: ['ap'] }))).toBeUndefined()
    expect(activeParkedApprovalId(task({ kind: 'Tasks', state: 'Waiting', references: ['ap'] }))).toBeUndefined()
    expect(activeParkedApprovalId(task({ kind: 'Approval', state: 'Waiting', references: ['ap'] }, '2026-11-06T00:00:00Z'))).toBeUndefined()
    expect(activeParkedApprovalId(task(undefined))).toBeUndefined()
    const failed = { metadata: { name: 'tk' }, status: { phase: 'Failed' as const, wait: { kind: 'Approval', state: 'Requested', references: ['ap'] } } }
    expect(activeParkedApprovalId(failed)).toBeUndefined()
  })
})
