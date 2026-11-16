import { describe, expect, it } from 'vitest'
import {
  attentionSourceNotes,
  deadlineText,
  groupByAttention,
  incompleteText,
  isAgentWorking,
  isTaskDerivedItem,
  mergeApprovalsSource,
  needsYouEmptyText,
  parseKanbanView,
  taskApprovalWaitSummary,
} from './attention-lanes'
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

  it('puts a Task waiting for approval by its own state in Needs you when the ranking has no item for it', () => {
    const items: AttentionItem[] = [
      { id: 'approval:u-ranked:ap', kind: 'approval', rank: 1, since: '', summary: 'Approval requested', task: { namespace: 'default', name: 'ranked', uid: 'u-ranked' },
        approval: { id: 'ap', source: 'worker', canApprove: true, canDecline: true } },
      { id: 'failed:u-broken', kind: 'failed', rank: 3, since: '', summary: 'Task failed', task: { namespace: 'default', name: 'broken', uid: 'u-broken' } },
    ]
    const since = '2026-11-16T00:00:00Z'
    const grouped = groupByAttention([
      task('ranked', { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True' }] }, 'u-ranked'),
      task('condition', { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True', lastTransitionTime: since }] }),
      task('worker', { phase: 'Running', message: 'waiting for approval of write' }),
      task('parked', { phase: 'Succeeded', wait: { kind: 'Approval', state: 'Waiting', references: ['ap-1'] } }),
      task('requested', { phase: 'Succeeded', wait: { kind: 'Approval', state: 'Requested', references: ['ap-2'] } }),
      // A requested wait after a failed turn is abandoned and takes no decision.
      task('abandoned', { phase: 'Failed', wait: { kind: 'Approval', state: 'Requested', references: ['ap-3'] } }),
      task('decided', { phase: 'Succeeded', wait: { kind: 'Approval', state: 'Resumed', references: ['ap-4'] } }),
      { ...task('deleting', { phase: 'Running', conditions: [{ type: 'WaitingForApproval', status: 'True' }] }),
        metadata: { name: 'deleting', namespace: 'default', uid: 'uid-deleting', deletionTimestamp: since } },
      task('busy', { phase: 'Running' }),
    ], items)
    const names = (list: AttentionItem[]) => list.map((item) => item.task?.name)
    // Ranked after the server's approvals and before lower-ranked kinds.
    expect(names(grouped.needsYou)).toEqual(['ranked', 'condition', 'worker', 'parked', 'requested', 'broken'])
    for (const item of grouped.needsYou.slice(1, 5)) {
      expect(isTaskDerivedItem(item)).toBe(true)
      expect(item.kind).toBe('approval')
      expect(item.summary).toBe(taskApprovalWaitSummary)
      expect(item.approval, 'no approval detail is derived from the Task').toBeUndefined()
    }
    expect(isTaskDerivedItem(grouped.needsYou[0])).toBe(false)
    expect(grouped.needsYou[1].since).toBe(since)
    expect(grouped.needsYou[1].task).toEqual({ namespace: 'default', name: 'condition', uid: 'uid-condition', phase: 'Running' })
    expect(grouped.working.map((t) => t.metadata.name)).toEqual(['deleting', 'busy'])
    expect(grouped.done.map((t) => t.metadata.name)).toEqual(['abandoned', 'decided'])
    // Without any derived card the server items are returned as they are.
    expect(groupByAttention([task('busy', { phase: 'Running' })], items).needsYou).toBe(items)
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
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'included' })).toEqual({ approvalsUnread: false, approvalsPartial: false })
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'notRequested' })).toEqual({ approvalsUnread: false, approvalsPartial: false })
    expect(attentionSourceNotes({ approvals: 'unavailable', monitors: 'included' })).toEqual({
      approvalsUnread: true, approvalsPartial: false, approvalsNote: expect.stringMatching(/execution event storage is not enabled/),
    })
    expect(attentionSourceNotes({ approvals: 'partial', monitors: 'included' })).toEqual({
      approvalsUnread: true, approvalsPartial: true, approvalsNote: expect.stringMatching(/^Some approvals could not be checked/),
    })
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'forbidden' }).monitorsNote).toMatch(/cannot list repository monitors/)
    expect(attentionSourceNotes({ approvals: 'included', monitors: 'unavailable' }).monitorsNote).toMatch(/monitor storage is not enabled/)
    // A response without a source state never reads as approvals checked.
    expect(attentionSourceNotes(undefined).approvalsUnread).toBe(true)
    expect(attentionSourceNotes(undefined).approvalsNote).toBe('Approvals could not be checked.')
  })

  it('qualifies the empty Needs you text for partial and unread approvals', () => {
    expect(needsYouEmptyText(attentionSourceNotes({ approvals: 'included', monitors: 'included' }))).toBe('Nothing needs you right now')
    expect(needsYouEmptyText(attentionSourceNotes({ approvals: 'partial', monitors: 'included' })))
      .toBe('Nothing else needs you right now, but some approvals could not be checked')
    expect(needsYouEmptyText(attentionSourceNotes({ approvals: 'unavailable', monitors: 'included' })))
      .toBe('Nothing else needs you right now, but approvals were not checked')
  })

  it('merges approval states across pages: unavailable over partial over included', () => {
    expect(mergeApprovalsSource('included', 'included')).toBe('included')
    expect(mergeApprovalsSource('included', 'partial')).toBe('partial')
    expect(mergeApprovalsSource('partial', 'included')).toBe('partial')
    expect(mergeApprovalsSource('partial', 'unavailable')).toBe('unavailable')
    expect(mergeApprovalsSource('unavailable', 'partial')).toBe('unavailable')
    expect(mergeApprovalsSource('included', undefined)).toBeUndefined()
  })
})
