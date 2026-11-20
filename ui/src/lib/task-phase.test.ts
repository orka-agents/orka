import { describe, expect, it } from 'vitest'
import { taskPhaseSchema } from '@/schemas/task'
import { isTaskInFlight, isTaskTerminal } from './task-phase'

describe('task phase lifecycle', () => {
  const expected = {
    Pending: { terminal: false, inFlight: true },
    Running: { terminal: false, inFlight: true },
    Finalizing: { terminal: false, inFlight: true },
    Succeeded: { terminal: true, inFlight: false },
    Failed: { terminal: true, inFlight: false },
    Cancelled: { terminal: true, inFlight: false },
    Scheduled: { terminal: false, inFlight: false },
  }

  it('classifies every schema phase', () => {
    expect(Object.keys(expected).sort()).toEqual([...taskPhaseSchema.options].sort())
    for (const phase of taskPhaseSchema.options) {
      expect(isTaskTerminal(phase), phase).toBe(expected[phase].terminal)
      expect(isTaskInFlight(phase), phase).toBe(expected[phase].inFlight)
    }
  })

  it('treats missing and unknown phases as neither', () => {
    for (const phase of [undefined, '', 'Unknown', 'succeeded']) {
      expect(isTaskTerminal(phase)).toBe(false)
      expect(isTaskInFlight(phase)).toBe(false)
    }
  })
})
