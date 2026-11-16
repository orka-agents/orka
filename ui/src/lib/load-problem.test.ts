import { describe, it, expect } from 'vitest'
import { ApiError } from '@/lib/api-client'
import { loadProblem, loadProblemText } from './load-problem'

describe('loadProblem', () => {
  const forbidden = new ApiError(403, '{"error":{"code":403,"message":"scope missing"}}')
  const failed = new ApiError(500, '{"error":{"code":500,"message":"store unavailable"}}')

  it('reports a 403 even when data already loaded', () => {
    expect(loadProblem(forbidden, true)).toEqual({ forbidden: true, message: 'scope missing' })
  })

  it('reports other failures only while nothing has loaded', () => {
    expect(loadProblem(failed, false)).toEqual({ forbidden: false, message: 'store unavailable' })
    expect(loadProblem(failed, true)).toBeUndefined()
    expect(loadProblem(null, false)).toBeUndefined()
  })

  it('describes the problem for the resource', () => {
    expect(loadProblemText('tasks', { forbidden: true, message: 'scope missing' })).toBe('Not authorized to list tasks (scope missing).')
    expect(loadProblemText('tasks', { forbidden: false, message: '' })).toBe('Could not load tasks.')
  })
})
