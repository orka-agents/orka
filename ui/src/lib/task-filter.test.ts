import { describe, it, expect } from 'vitest'
import { compareTasksNewestFirst, matchesTaskFilter } from './task-filter'
import type { Task } from '@/schemas/task'

function task(name: string, type: Task['spec']['type'], phase: string, creationTimestamp?: string): Task {
  return {
    metadata: { name, namespace: 'default', creationTimestamp },
    spec: { type },
    status: { phase } as Task['status'],
  }
}

describe('matchesTaskFilter', () => {
  const container = task('build-image', 'container', 'Running')
  const ai = task('summarize', 'ai', 'Succeeded')

  it('matches type and phase as words, not inside other words', () => {
    // "container" contains "ai"; that must not make every Container match.
    expect(matchesTaskFilter(container, 'ai')).toBe(false)
    expect(matchesTaskFilter(ai, 'ai')).toBe(true)
    expect(matchesTaskFilter(ai, 'AI')).toBe(true)
  })

  it('accepts the start of a type or phase', () => {
    expect(matchesTaskFilter(container, 'run')).toBe(true)
    expect(matchesTaskFilter(ai, 'succ')).toBe(true)
    expect(matchesTaskFilter(ai, 'ceeded')).toBe(false)
  })

  it('still matches anywhere in the name', () => {
    expect(matchesTaskFilter(container, 'image')).toBe(true)
    expect(matchesTaskFilter(task('chain-repair', 'container', 'Failed'), 'ai')).toBe(true)
  })

  it('requires every term to match', () => {
    expect(matchesTaskFilter(container, 'container running')).toBe(true)
    expect(matchesTaskFilter(container, 'container failed')).toBe(false)
    expect(matchesTaskFilter(container, '   ')).toBe(true)
  })
})

describe('compareTasksNewestFirst', () => {
  it('orders by creation time, newest first, with undated tasks last', () => {
    const tasks = [
      task('old', 'ai', 'Succeeded', '2026-01-01T00:00:00Z'),
      task('undated', 'ai', 'Succeeded'),
      task('new', 'ai', 'Succeeded', '2026-03-01T00:00:00Z'),
    ]
    expect([...tasks].sort(compareTasksNewestFirst).map((t) => t.metadata.name)).toEqual(['new', 'old', 'undated'])
  })
})
