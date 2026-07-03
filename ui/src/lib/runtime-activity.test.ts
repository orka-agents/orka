import { describe, it, expect } from 'vitest'
import {
  UNASSIGNED_AGENT,
  isLiveTask,
  taskKey,
  compareByActivity,
  selectActiveTask,
  groupTasksByAgent,
  elapsedSeconds,
  formatElapsed,
} from './runtime-activity'
import type { Task } from '@/schemas/task'

function task(name: string, overrides: Partial<Task> = {}): Task {
  return {
    metadata: { name, namespace: 'default', uid: name, ...overrides.metadata },
    spec: { type: 'agent', ...overrides.spec },
    status: overrides.status ?? { phase: 'Running' },
  }
}

describe('runtime-activity', () => {
  it('isLiveTask true only for Running', () => {
    expect(isLiveTask(task('a', { status: { phase: 'Running' } }))).toBe(true)
    expect(isLiveTask(task('b', { status: { phase: 'Succeeded' } }))).toBe(false)
    expect(isLiveTask(task('c', { status: {} }))).toBe(false)
  })

  it('taskKey prefers uid then falls back to name', () => {
    expect(taskKey(task('a', { metadata: { name: 'a', uid: 'u1' } }))).toBe('u1')
    expect(taskKey(task('a', { metadata: { name: 'a', uid: '' } }))).toBe('a')
  })

  it('selectActiveTask returns null when nothing running', () => {
    expect(selectActiveTask([])).toBeNull()
    expect(selectActiveTask([task('s', { status: { phase: 'Succeeded' } })])).toBeNull()
  })

  it('selectActiveTask prefers most-recent event activity time', () => {
    const tasks = [task('old'), task('hot')]
    expect(selectActiveTask(tasks, { hot: 2000, old: 1000 })?.metadata.name).toBe('hot')
  })

  it('does not let stale cached event activity outrank a newer start time', () => {
    const stale = task('stale', { status: { phase: 'Running', startTime: '2026-06-28T10:00:00Z' } })
    const newer = task('newer', { status: { phase: 'Running', startTime: '2026-06-28T12:00:00Z' } })
    expect(selectActiveTask([stale, newer], { stale: Date.parse('2026-06-28T11:00:00Z') })?.metadata.name).toBe('newer')
  })

  it('selectActiveTask falls back to newest startTime then name when no seq', () => {
    const a = task('a', { status: { phase: 'Running', startTime: '2026-06-28T10:00:00Z' } })
    const b = task('b', { status: { phase: 'Running', startTime: '2026-06-28T11:00:00Z' } })
    expect(selectActiveTask([a, b])?.metadata.name).toBe('b')
  })

  it('handles a running task with no startTime via stable fallback', () => {
    const withStart = task('z', { status: { phase: 'Running', startTime: '2026-06-28T10:00:00Z' } })
    const noStart = task('a', { status: { phase: 'Running' } })
    // started task ranks above startTime-less; no NaN/undefined comparison
    expect(selectActiveTask([noStart, withStart])?.metadata.name).toBe('z')
    // two startTime-less running tasks fall back to ascending name order
    const sorted = [task('b', { status: { phase: 'Running' } }), task('a', { status: { phase: 'Running' } })]
      .sort((x, y) => compareByActivity(x, y))
    expect(sorted.map((t) => t.metadata.name)).toEqual(['a', 'b'])
  })

  it('groupTasksByAgent buckets unassigned last', () => {
    const groups = groupTasksByAgent([
      task('x', { spec: { type: 'agent' } }),
      task('y', { spec: { type: 'agent', agentRef: { name: 'alpha' } } }),
    ])
    expect(groups[0].agent).toBe('alpha')
    expect(groups[groups.length - 1].agent).toBe(UNASSIGNED_AGENT)
  })

  it('elapsedSeconds null without startTime, value with', () => {
    expect(elapsedSeconds(task('a', { status: { phase: 'Running' } }), 1_000_000)).toBeNull()
    const t = task('b', { status: { phase: 'Running', startTime: new Date(0).toISOString() } })
    expect(elapsedSeconds(t, 5000)).toBe(5)
  })

  it('formatElapsed renders dash for null and compact units', () => {
    expect(formatElapsed(null)).toBe('—')
    expect(formatElapsed(5)).toBe('5s')
    expect(formatElapsed(125)).toBe('2m 5s')
    expect(formatElapsed(3725)).toBe('1h 2m')
  })
})
