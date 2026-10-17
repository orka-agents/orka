import { isForbiddenError } from '@/lib/api-client'

/** Why a view shows a message instead of its data. */
export interface LoadProblem {
  forbidden: boolean
  message: string
}

/**
 * The load failure a view should show instead of its data: a 403 always,
 * since the token cannot see the data any more, and any other failure only
 * while nothing has loaded, so a failed background refresh keeps what loaded.
 */
export function loadProblem(error: unknown, hasData: boolean): LoadProblem | undefined {
  if (!error) return undefined
  const forbidden = isForbiddenError(error)
  if (!forbidden && hasData) return undefined
  return { forbidden, message: error instanceof Error ? error.message.trim() : '' }
}

/** One-line text for a load problem, e.g. "Not authorized to list tasks (…)". */
export function loadProblemText(resource: string, problem: LoadProblem): string {
  const detail = problem.message ? ` (${problem.message})` : ''
  return problem.forbidden ? `Not authorized to list ${resource}${detail}.` : `Could not load ${resource}${detail}.`
}
