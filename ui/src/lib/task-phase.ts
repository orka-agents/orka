import type { TaskPhase } from '@/schemas/task'

/**
 * The Task phase lifecycle, in one place. Mirrors the controller:
 *
 *   Pending → Running → Finalizing → Succeeded | Failed | Cancelled
 *   Scheduled (a recurring parent; it spawns runs and never completes)
 *
 * Unknown or missing phases are neither terminal nor in flight.
 */

/** Final phases; matches Go `TaskPhase.IsTerminal()`. */
export const TERMINAL_TASK_PHASES: readonly TaskPhase[] = ['Succeeded', 'Failed', 'Cancelled']

/** The Task is over and will not run again. */
export function isTaskTerminal(phase?: TaskPhase | string): boolean {
  return (TERMINAL_TASK_PHASES as readonly string[]).includes(phase ?? '')
}

/**
 * The Task holds or awaits execution, so its status, trace, approvals and
 * artifacts can still change. Finalizing is included: publication happens
 * there. Matches the controller's concurrency accounting.
 */
export function isTaskInFlight(phase?: TaskPhase | string): boolean {
  return phase === 'Pending' || phase === 'Running' || phase === 'Finalizing'
}
