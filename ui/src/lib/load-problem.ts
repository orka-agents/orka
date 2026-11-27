import { isForbiddenError } from '@/lib/api-client'

export interface LoadProblem {
  forbidden: boolean
  message: string
}

export function loadProblem(error: unknown, hasData: boolean): LoadProblem | undefined {
  if (!error) return undefined
  const forbidden = isForbiddenError(error)
  if (!forbidden && hasData) return undefined
  return { forbidden, message: error instanceof Error ? error.message.trim() : '' }
}

export function loadProblemText(resource: string, problem: LoadProblem): string {
  const detail = problem.message ? ` (${problem.message})` : ''
  return problem.forbidden ? `Not authorized to list ${resource}${detail}.` : `Could not load ${resource}${detail}.`
}
