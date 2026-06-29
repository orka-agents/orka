import { describe, it, expect } from 'vitest'
import { initialSimState, stepSim, injectFailure, resetSim } from './runtime-simulator'

describe('runtime-simulator', () => {
  it('initial state is one pending task, one event, not running', () => {
    const s = initialSimState()
    expect(s.tasks).toHaveLength(1)
    expect(s.tasks[0].status?.phase).toBe('Pending')
    expect(s.step).toBe(0)
    expect(s.tasks[0].metadata.namespace).toBe('simulator')
  })

  it('step advances phase deterministically', () => {
    const s1 = stepSim(initialSimState())
    expect(s1.step).toBe(1)
    expect(s1.tasks[0].status?.phase).toBe('Running')
    const s3 = stepSim(stepSim(s1))
    expect(s3.tasks[0].status?.phase).toBe('Succeeded')
    expect(s3.events).toHaveLength(4)
  })

  it('injectFailure marks Failed with error event', () => {
    const s = injectFailure(initialSimState())
    expect(s.tasks[0].status?.phase).toBe('Failed')
    expect(s.events[s.events.length - 1].severity).toBe('error')
  })

  it('reset returns to initial', () => {
    expect(resetSim()).toEqual(initialSimState())
  })

  it('fixtures use the simulator namespace, never production', () => {
    expect(stepSim(initialSimState()).tasks[0].metadata.namespace).toBe('simulator')
  })
})
