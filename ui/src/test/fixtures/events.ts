import type { ExecutionEvent } from '@/schemas/execution-event'

let seqCounter = 0

// Build an execution event fixture with sensible defaults. Pass overrides for the
// fields a given test cares about.
export function makeEvent(overrides: Partial<ExecutionEvent> = {}): ExecutionEvent {
  seqCounter += 1
  return {
    id: `evt-${seqCounter}`,
    namespace: 'default',
    streamType: 'task',
    streamID: 'tk',
    seq: seqCounter,
    type: 'TaskStarted',
    severity: 'info',
    createdAt: '2026-06-13T00:00:00.000Z',
    ...overrides,
  }
}

export function resetEventSeq() {
  seqCounter = 0
}
