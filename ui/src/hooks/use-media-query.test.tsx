import { describe, it, expect, afterEach, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { useIsMobile, useMediaQuery } from './use-media-query'

type Listener = () => void

function installMatchMedia(matches: boolean) {
  const listeners = new Set<Listener>()
  const list = {
    matches,
    addEventListener: (_: string, fn: Listener) => listeners.add(fn),
    removeEventListener: (_: string, fn: Listener) => listeners.delete(fn),
  }
  window.matchMedia = vi.fn().mockImplementation(() => list) as unknown as typeof window.matchMedia
  return {
    set(next: boolean) {
      list.matches = next
      listeners.forEach((fn) => fn())
    },
  }
}

describe('useMediaQuery', () => {
  const original = window.matchMedia
  afterEach(() => {
    window.matchMedia = original
  })

  it('returns false when matchMedia is unavailable', () => {
    // @ts-expect-error simulate an environment without matchMedia
    window.matchMedia = undefined
    const { result } = renderHook(() => useMediaQuery('(max-width: 767px)'))
    expect(result.current).toBe(false)
  })

  it('tracks the match state and updates on change', () => {
    const media = installMatchMedia(true)
    const { result } = renderHook(() => useIsMobile())
    expect(result.current).toBe(true)
    act(() => media.set(false))
    expect(result.current).toBe(false)
  })
})
