import { describe, expect, it } from 'vitest'
import { attentionSourceNotes, deadlineText, groupByAttention, incompleteText, isAgentWorking, parseKanbanView } from './attention-lanes'
import type { AttentionItem } from '@/schemas/attention'
import type { Task } from '@/schemas/task'

function task(name: string, status: Task['status'], uid = `uid-${name}`): Task {
  return { metadata: { name, namespace: 'default', uid }, spec: { type: 'agent' }, status }
}

describe('attention lanes', () => {
  it('counts queued, running and waiting Tasks as agent working', () => {
    for (const phase of ['Pending', 'Scheduled', 'Running', 'Finalizing'] as const) {
      expect(isAgentWorking(task('t', { phase }))).toBe(true)
    }
    expect(isAgentWorking(task('t', undefined))).toBe(true)
    expect(isAgentWorking(task('t', { phase: 'Failed', conditions: [{ type: 'WaitingForCapacity', status: 'True' }] }))).toBe(true)
    expect(isAgentWorking(task('t', { phase: 'Succeeded', wait: { kind: 'Tasks', state: 'Waiting' } }))).toBe(true)
    expect(isAgentWorking(task('t', { phase: 'Succeeded', wait: { kind: 'Timer', state: 'Resuming' } }))).toBe(true)
    expect(isAgentWorking(task('t', { phase: 'Succeeded', wait: { kind: 'Timer', state: 'Resumed' } }))).toBe(false)
    // A parked approval is the person's move, never the agent's.
    expect(isAgentWorking(task('t', { phase: 'Succeeded', wait: { kind: 'Approval', state: 'Waiting' } }))).toBe(false)
    expect(isAgentWorking(task('t', { phase: 'Cancelled' }))).toBe(false)
  })

  it('takes Needs you only from attention items and keeps those Tasks out of the other lanes', () => {
    const items: AttentionItem[] = [
      { id: 'a', kind: 'approval', rank: 1, since: '', summary: 'Approval requested', task: { namespace: 'default', name: 'waiting', uid: 'uid-waiting' } },
      { id: 'm', kind: 'monitorBlocked', rank: 5, since: '', summary: 'Monitor blocked', monitor: { name: 'repo', actionID: 'x' } },
    ]
    const grouped = groupByAttention([
      task('waiting', { phase: 'Running' }),
      task('running', { phase: 'Running' }),
      task('failed-old', { phase: 'Failed' }),
    ], items)
    expect(grouped.needsYou).toBe(items)
    expect(grouped.working.map((t) => t.metadata.name)).toEqual(['running'])
    expect(grouped.done.map((t) => t.metadata.name)).toEqual(['failed-old'])
  })

  it('matches Tasks by UID so a same-named replacement is not flagged', () => {
    const items: AttentionItem[] = [
      { id: 'a', kind: 'failed', rank: 3, since: '', summary: 'Task failed', task: { namespace: 'default', name: 'reused', uid: 'old-uid' } },
    ]
    const grouped = groupByAttention([task('reused', { phase: 'Running' }, 'new-uid')], items)
    expect(grouped.working).toHaveLength(1)
  })

  it('parses the board view and formats deadlines and incomplete scans', () => {
    expect(parseKanbanView('attention')).toBe('attention')
    expect(parseKanbanView('phase')).toBe('phase')
    expect(parseKanbanView('other')).toBe('phase')
    expect(parseKanbanView(undefined)).toBe('phase')
    const now = Date.parse('2026-11-16T00:00:00Z')
    expect(deadlineText('2026-11-16T00:12:00Z', now)).toBe('due in 12m')
    expect(deadlineText('2026-11-15T23:59:00Z', now)).toBe('overdue')
    expect(deadlineText(undefined, now)).toBeUndefined()
    expect(incompleteText(['taskScanLimit'])).toMatch(/may be incomplete: only the first Tasks/)
    expect(incompleteText(['somethingNew'])).toContain('somethingNew')
  })

  it('reports skipped sources apart from scan limits', () => {
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'included' })).toEqual({ approvalsUnread: false })
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'notRequested' })).toEqual({ approvalsUnread: false })
    expect(attentionSourceNotes({ approvals: 'unavailable', monitors: 'included' }).approvalsUnread).toBe(true)
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'forbidden' }).monitorsNote).toMatch(/cannot list repository monitors/)
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'unavailable' }).monitorsNote).toMatch(/monitor storage is not enabled/)
    // A response without a source state never reads as approvals checked.
    expect(attentionSourceNotes(undefined).approvalsUnread).toBe(true)
  })
})
