import { afterEach, describe, expect, it, vi } from 'vitest'

function memoryStorage(entries: Record<string, string>) {
  const items = new Map(Object.entries(entries))
  return {
    getItem: (key: string) => items.get(key) ?? null,
    setItem: (key: string, value: string) => void items.set(key, value),
    removeItem: (key: string) => void items.delete(key),
  }
}

async function loadStore(storage: ReturnType<typeof memoryStorage>) {
  vi.resetModules()
  vi.stubGlobal('localStorage', storage)
  const { useUIStore } = await import('./ui')
  return useUIStore
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

describe('persisted namespace', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('keeps a typed namespace across reloads', async () => {
    const storage = memoryStorage({})
    const first = await loadStore(storage)
    first.getState().adoptCallerNamespace('team-a')
    first.getState().setNamespace('team-b')

    const reloaded = await loadStore(storage)
    expect(reloaded.getState().namespace).toBe('team-b')
    reloaded.getState().adoptCallerNamespace('team-a')
    expect(reloaded.getState().namespace).toBe('team-b')
    expect(reloaded.getState().recentNamespaces).toEqual(['team-b', 'team-a'])
  })

  it('carries a version 1 namespace into the recent list', async () => {
    const store = await loadStore(memoryStorage({ 'orka-ui': JSON.stringify({ state: { theme: 'dark', namespace: 'default' }, version: 1 }) }))
    expect(store.getState()).toMatchObject({ namespace: 'default', recentNamespaces: ['default'], callerNamespace: null, theme: 'dark' })
  })
})
