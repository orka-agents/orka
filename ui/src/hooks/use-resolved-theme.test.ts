import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useUIStore } from '@/stores/ui'
import { useResolvedTheme } from './use-resolved-theme'

// jsdom has no matchMedia; this one reports the system theme and announces changes.
function stubSystemTheme(dark: boolean) {
  const listeners = new Set<() => void>()
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      get matches() { return query === '(prefers-color-scheme: dark)' && dark },
      media: query,
      addEventListener: (_: string, listener: () => void) => listeners.add(listener),
      removeEventListener: (_: string, listener: () => void) => listeners.delete(listener),
    }),
  })
  return (next: boolean) => {
    dark = next
    listeners.forEach((listener) => listener())
  }
}

describe('useResolvedTheme', () => {
  afterEach(() => {
    delete (window as { matchMedia?: unknown }).matchMedia
  })

  it('follows the system while the user picks System, including later changes', () => {
    const setSystemDark = stubSystemTheme(true)
    useUIStore.setState({ theme: 'system' })
    const { result } = renderHook(() => useResolvedTheme())
    expect(result.current).toBe('dark')
    act(() => setSystemDark(false))
    expect(result.current).toBe('light')
  })

  it('keeps an explicit pick whatever the system says', () => {
    stubSystemTheme(true)
    useUIStore.setState({ theme: 'light' })
    const { result } = renderHook(() => useResolvedTheme())
    expect(result.current).toBe('light')
    act(() => useUIStore.setState({ theme: 'dark' }))
    expect(result.current).toBe('dark')
  })

  it('is light when the browser cannot report a system theme', () => {
    useUIStore.setState({ theme: 'system' })
    const { result } = renderHook(() => useResolvedTheme())
    expect(result.current).toBe('light')
  })
})
