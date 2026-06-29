import type { Task } from '@/schemas/task'
import type { ExecutionEvent } from '@/schemas/execution-event'

/**
 * Deterministic, in-memory simulator state — DEV/demo ONLY. It produces fake
 * Task + ExecutionEvent fixtures and NEVER calls any Orka API. Production canvas
 * code does not import this; the only entry point is a DEV-gated route. All
 * transitions are pure so tests can assert determinism.
 */
export interface SimState {
  tasks: Task[]
  events: ExecutionEvent[]
  step: number
  running: boolean
}

const PHASES = ['Pending', 'Running', 'Running', 'Succeeded'] as const

function fixtureTask(step: number): Task {
  const phase = PHASES[Math.min(step, PHASES.length - 1)]
  return {
    metadata: { name: 'sim-task', namespace: 'simulator', uid: 'sim-uid' },
    spec: { type: 'agent', agentRef: { name: 'sim-agent' } },
    status: { phase, startTime: new Date(0).toISOString(), message: `step ${step}` },
  }
}

function fixtureEvent(step: number): ExecutionEvent {
  return {
    id: `sim-${step}`, namespace: 'simulator', streamType: 'task', streamID: 'sim-task',
    seq: step, type: step === 0 ? 'TaskCreated' : 'ToolCallCompleted', severity: 'info',
    summary: `simulated step ${step}`, createdAt: new Date(0).toISOString(),
  }
}

export function initialSimState(): SimState {
  return { tasks: [fixtureTask(0)], events: [fixtureEvent(0)], step: 0, running: false }
}

export function stepSim(s: SimState): SimState {
  const step = s.step + 1
  return { ...s, step, tasks: [fixtureTask(step)], events: [...s.events, fixtureEvent(step)] }
}

export function injectFailure(s: SimState): SimState {
  return {
    ...s,
    tasks: [{ ...s.tasks[0], status: { ...s.tasks[0].status, phase: 'Failed', message: 'injected failure' } }],
    events: [...s.events, { ...fixtureEvent(s.step + 1), type: 'TaskFailed', severity: 'error', summary: 'injected failure' }],
    step: s.step + 1,
  }
}

export function resetSim(): SimState {
  return initialSimState()
}
