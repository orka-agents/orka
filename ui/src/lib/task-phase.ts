import type { TaskPhase } from '@/schemas/task'

export const TERMINAL_TASK_PHASES: readonly TaskPhase[] = ['Succeeded', 'Failed', 'Cancelled']

export function isTaskTerminal(phase?: TaskPhase | string): boolean {
  return (TERMINAL_TASK_PHASES as readonly string[]).includes(phase ?? '')
}

export function isTaskInFlight(phase?: TaskPhase | string): boolean {
  return phase === 'Pending' || phase === 'Running' || phase === 'Finalizing'
}
