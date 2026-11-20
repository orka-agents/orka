import { describe, it, expect } from 'vitest'
import { statusProblem } from './status-problem'

const notReady = (reason: string, message?: string) => [{ type: 'Ready', status: 'False', reason, message }]

describe('statusProblem', () => {
  it('reports the Ready=False reason and message of an Error phase', () => {
    expect(statusProblem({ phase: 'Error', conditions: notReady('InvalidSchedule', 'expected exactly 5 fields') }))
      .toEqual({ reason: 'InvalidSchedule', message: 'expected exactly 5 fields' })
    expect(statusProblem({ phase: 'Error', conditions: notReady('ScanFailed') })).toEqual({ reason: 'ScanFailed' })
  })

  it.each([
    ['no status', undefined],
    ['a not-ready phase that names itself', { phase: 'Scanning', conditions: notReady('Scanning', 'scan is running') }],
    ['a suspended phase', { phase: 'Suspended', conditions: notReady('Suspended') }],
    ['an Error phase without conditions', { phase: 'Error' }],
    ['an Error phase whose Ready condition is True', { phase: 'Error', conditions: [{ type: 'Ready', status: 'True', reason: 'ScanSucceeded' }] }],
    ['an Error phase with only other conditions', { phase: 'Error', conditions: [{ type: 'Synced', status: 'False', reason: 'Other' }] }],
  ])('reports nothing for %s', (_label, status) => {
    expect(statusProblem(status)).toBeUndefined()
  })
})
