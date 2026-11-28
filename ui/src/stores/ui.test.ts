import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore, persistedUIState, migrateUIState, namespaceOptions, RECENT_NAMESPACE_LIMIT } from './ui'

describe('useUIStore', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default', recentNamespaces: [], callerNamespace: null })
    document.documentElement.classList.remove('dark')
  })

  it('has correct initial state', () => {
    const state = useUIStore.getState()
    expect(state.sidebarCollapsed).toBe(false)
    expect(state.theme).toBe('light')
    expect(state.namespace).toBe('default')
  })

  it('follows the system theme until the user picks one', () => {
    expect(useUIStore.getInitialState().theme).toBe('system')
  })

  it('toggleSidebar toggles the value', () => {
    expect(useUIStore.getState().sidebarCollapsed).toBe(false)
    useUIStore.getState().toggleSidebar()
    expect(useUIStore.getState().sidebarCollapsed).toBe(true)
    useUIStore.getState().toggleSidebar()
    expect(useUIStore.getState().sidebarCollapsed).toBe(false)
  })

  it('setTheme records the pick; the root applies it to the page', () => {
    for (const theme of ['dark', 'system', 'light'] as const) {
      useUIStore.getState().setTheme(theme)
      expect(useUIStore.getState().theme).toBe(theme)
    }
    expect(document.documentElement.classList.contains('dark')).toBe(false)
  })

  it('setNamespace updates namespace', () => {
    useUIStore.getState().setNamespace('production')
    expect(useUIStore.getState().namespace).toBe('production')
  })

  it('setNamespace remembers recent namespaces, newest first and capped', () => {
    for (let index = 0; index <= RECENT_NAMESPACE_LIMIT; index++) useUIStore.getState().setNamespace(`team-${index}`)
    useUIStore.getState().setNamespace('team-5')
    const recent = useUIStore.getState().recentNamespaces
    expect(recent).toHaveLength(RECENT_NAMESPACE_LIMIT)
    expect(recent[0]).toBe('team-5')
    expect(recent.filter((entry) => entry === 'team-5')).toHaveLength(1)
    expect(recent).not.toContain('team-0')
  })

  it('setNamespace ignores a name that is not a DNS-1123 label', () => {
    for (const name of ['', 'Team-A', 'team_a', '-team', 'a'.repeat(64), 'team.a']) {
      useUIStore.getState().setNamespace(name)
    }
    expect(useUIStore.getState().namespace).toBe('default')
    expect(useUIStore.getState().recentNamespaces).toEqual([])
  })

  it("adoptCallerNamespace switches to a new caller's namespace once", () => {
    useUIStore.getState().adoptCallerNamespace('team-a')
    expect(useUIStore.getState()).toMatchObject({ namespace: 'team-a', callerNamespace: 'team-a', recentNamespaces: ['team-a'] })

    useUIStore.getState().setNamespace('team-b')
    useUIStore.getState().adoptCallerNamespace('team-a')
    expect(useUIStore.getState().namespace).toBe('team-b')

    useUIStore.getState().adoptCallerNamespace('team-c')
    expect(useUIStore.getState().namespace).toBe('team-c')
  })

  it('adoptCallerNamespace forgets the caller when it has no namespace', () => {
    useUIStore.getState().adoptCallerNamespace('team-a')
    useUIStore.getState().setNamespace('team-b')
    useUIStore.getState().adoptCallerNamespace(undefined)
    expect(useUIStore.getState()).toMatchObject({ namespace: 'team-b', callerNamespace: null })
    useUIStore.getState().adoptCallerNamespace('team-a')
    expect(useUIStore.getState().namespace).toBe('team-a')
  })
})

describe('namespaceOptions', () => {
  it("offers the caller's namespace, the current one and recent ones", () => {
    expect(namespaceOptions({ namespace: 'team-b', recentNamespaces: ['team-c', 'team-b'], callerNamespace: 'team-a' }))
      .toEqual(['team-a', 'team-b', 'team-c'])
  })

  it('is no longer only default and orka-system', () => {
    expect(namespaceOptions({ namespace: 'orka-system', recentNamespaces: [], callerNamespace: 'team-a' }))
      .toEqual(['orka-system', 'team-a'])
    expect(namespaceOptions({ namespace: 'team-x', recentNamespaces: [], callerNamespace: null })).toEqual(['team-x'])
  })
})

describe('migrateUIState', () => {
  it("moves the old default 'light' to 'system' and keeps every other choice", () => {
    expect(migrateUIState({ theme: 'light', namespace: 'team-a' }, 0)).toEqual({ theme: 'system', namespace: 'team-a', recentNamespaces: ['team-a'] })
    expect(migrateUIState({ theme: 'dark' }, 0)).toEqual({ theme: 'dark' })
    expect(migrateUIState({ theme: 'light' }, 1)).toEqual({ theme: 'light' })
    expect(migrateUIState(undefined, 0)).toEqual({})
  })

  it('seeds recent namespaces from the stored namespace before version 2', () => {
    expect(migrateUIState({ namespace: 'team-a' }, 1)).toEqual({ namespace: 'team-a', recentNamespaces: ['team-a'] })
    expect(migrateUIState({ namespace: 'Not Valid' }, 1)).toEqual({ namespace: 'Not Valid' })
    expect(migrateUIState({ namespace: 'team-a', recentNamespaces: ['team-b'] }, 2)).toEqual({ namespace: 'team-a', recentNamespaces: ['team-b'] })
  })
})

describe('mobileSidebarOpen', () => {
  it('is not part of the persisted state', () => {
    useUIStore.setState({ mobileSidebarOpen: true })
    expect(Object.keys(persistedUIState(useUIStore.getState()))).toEqual(['sidebarCollapsed', 'theme', 'namespace', 'recentNamespaces', 'callerNamespace'])
  })
})
