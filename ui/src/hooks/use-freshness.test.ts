import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useFreshness } from './use-freshness'

describe('useFreshness', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('is false on initial mount (a fresh mount is not a change)', () => {
    const { result } = renderHook(({ v }) => useFreshness(v), {
      initialProps: { v: 'a' },
    })
    expect(result.current).toBe(false)
  })

  it('flips to true immediately when the tracked value changes', () => {
    const { result, rerender } = renderHook(({ v }) => useFreshness(v), {
      initialProps: { v: 'a' },
    })
    expect(result.current).toBe(false)
    act(() => rerender({ v: 'b' }))
    expect(result.current).toBe(true)
  })

  it('decays back to false after the decay window', () => {
    const { result, rerender } = renderHook(({ v }) => useFreshness(v, 1200), {
      initialProps: { v: 'a' },
    })
    act(() => rerender({ v: 'b' }))
    expect(result.current).toBe(true)

    act(() => {
      vi.advanceTimersByTime(1199)
    })
    expect(result.current).toBe(true)

    act(() => {
      vi.advanceTimersByTime(1)
    })
    expect(result.current).toBe(false)
  })

  it('does not flip when the value is unchanged across renders', () => {
    const { result, rerender } = renderHook(({ v }) => useFreshness(v), {
      initialProps: { v: 'a' },
    })
    act(() => rerender({ v: 'a' }))
    expect(result.current).toBe(false)
  })

  it('re-arms the decay timer on a subsequent change', () => {
    const { result, rerender } = renderHook(({ v }) => useFreshness(v, 1000), {
      initialProps: { v: 'a' },
    })
    act(() => rerender({ v: 'b' }))
    act(() => vi.advanceTimersByTime(800))
    expect(result.current).toBe(true)

    // A new change restarts the window.
    act(() => rerender({ v: 'c' }))
    act(() => vi.advanceTimersByTime(800))
    expect(result.current).toBe(true)

    act(() => vi.advanceTimersByTime(200))
    expect(result.current).toBe(false)
  })

  it('honors a custom decay duration', () => {
    const { result, rerender } = renderHook(({ v }) => useFreshness(v, 300), {
      initialProps: { v: 1 },
    })
    act(() => rerender({ v: 2 }))
    expect(result.current).toBe(true)
    act(() => vi.advanceTimersByTime(300))
    expect(result.current).toBe(false)
  })
})
