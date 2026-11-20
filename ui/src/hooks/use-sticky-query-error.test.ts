import { describe, it, expect } from 'vitest'
import { renderHook } from '@testing-library/react'
import { useStickyQueryError } from './use-sticky-query-error'

describe('useStickyQueryError', () => {
  it('keeps a failed first load through a poll that resets the query to pending', () => {
    const failure = new Error('store unavailable')
    const { result, rerender } = renderHook((state: { data: unknown; error: Error | null; errorUpdatedAt: number }) => useStickyQueryError(state), {
      initialProps: { data: undefined, error: failure, errorUpdatedAt: 100 },
    })
    expect(result.current).toBe(failure)

    // Polling again clears `error` while nothing has loaded.
    rerender({ data: undefined, error: null, errorUpdatedAt: 100 })
    expect(result.current).toBe(failure)

    // A successful load ends it.
    rerender({ data: { items: [] }, error: null, errorUpdatedAt: 100 })
    expect(result.current).toBeNull()
  })

  it('does not resurrect an error from an earlier query', () => {
    const { result, rerender } = renderHook((state: { data: unknown; error: Error | null; errorUpdatedAt: number }) => useStickyQueryError(state), {
      initialProps: { data: undefined, error: new Error('old'), errorUpdatedAt: 100 },
    })
    // A different query (e.g. another namespace) has its own error timestamp.
    rerender({ data: undefined, error: null, errorUpdatedAt: 0 })
    expect(result.current).toBeNull()
  })
})
