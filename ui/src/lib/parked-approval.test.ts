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

  it('stops showing a parked approval once its wait deadline passes', () => {
    const now = Date.parse('2026-11-16T12:00:00Z')
    const task = (deadline: string) => ({
      metadata: { name: 'tk' },
      status: { phase: 'Succeeded' as const, wait: { kind: 'Approval', state: 'Waiting', references: ['ap'], deadline } },
    })
    expect(activeParkedApprovalId(task('2026-11-16T12:05:00Z'), now)).toBe('ap')
    expect(activeParkedApprovalId(task('2026-11-16T12:00:00Z'), now)).toBeUndefined()
    expect(activeParkedApprovalId(task('2026-11-16T11:00:00Z'), now)).toBeUndefined()
    expect(activeParkedApprovalId(task('not-a-time'), now)).toBe('ap')
  })
})
