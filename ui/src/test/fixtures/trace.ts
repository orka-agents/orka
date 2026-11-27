import type { TaskTrace } from '@/schemas/execution-event'

export function makeTrace(overrides: Partial<TaskTrace> = {}): TaskTrace {
  return {
    task: { namespace: 'default', name: 'tk', phase: 'Succeeded', resultAvailable: false },
    latestSeq: 0,
    generatedAt: '2026-06-13T00:00:00.000Z',
    timeline: [],
    modelRequests: [],
    toolCalls: [],
    childTasks: [],
    workspace: [],
    artifacts: [],
    errors: [],
    warnings: [],
    ...overrides,
  }
}
