import { describe, it, expect } from 'vitest'
import {
  isLiveTask,
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
