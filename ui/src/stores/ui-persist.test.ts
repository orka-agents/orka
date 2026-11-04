import { afterEach, describe, expect, it, vi } from 'vitest'

// Loads the store through the real persist middleware, so hydration runs the
// same migration rule as the pre-paint script in index.html (index-html.test.ts).
function memoryStorage(entries: Record<string, string>) {
  const items = new Map(Object.entries(entries))
  return {
    getItem: (key: string) => items.get(key) ?? null,
    setItem: (key: string, value: string) => void items.set(key, value),
    removeItem: (key: string) => void items.delete(key),
  }
}

async function hydratedTheme(stored?: { theme: string; version?: number }) {
  vi.resetModules()
  vi.stubGlobal('localStorage', memoryStorage(stored
    ? { 'orka-ui': JSON.stringify({ state: { theme: stored.theme }, ...(stored.version !== undefined && { version: stored.version }) }) }
    : {}))
  const { useUIStore } = await import('./ui')
  return useUIStore.getState().theme
}

describe('persisted theme', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('starts a first visit on System', async () => {
    expect(await hydratedTheme()).toBe('system')
  })

  it('moves a stored light from before version 1 to System and keeps other picks', async () => {
    expect(await hydratedTheme({ theme: 'light', version: 0 })).toBe('system')
    expect(await hydratedTheme({ theme: 'dark', version: 0 })).toBe('dark')
    expect(await hydratedTheme({ theme: 'light', version: 1 })).toBe('light')
    expect(await hydratedTheme({ theme: 'light' })).toBe('light')
  })
})
