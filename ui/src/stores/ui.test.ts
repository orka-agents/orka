import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore, persistedUIState, migrateUIState } from './ui'

describe('useUIStore', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
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

  it('setSidebarCollapsed sets the value explicitly', () => {
    useUIStore.getState().setSidebarCollapsed(true)
    expect(useUIStore.getState().sidebarCollapsed).toBe(true)
    useUIStore.getState().setSidebarCollapsed(true)
    expect(useUIStore.getState().sidebarCollapsed).toBe(true)
    useUIStore.getState().setSidebarCollapsed(false)
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
})

describe('migrateUIState', () => {
  it("moves the old default 'light' to 'system' and keeps every other choice", () => {
    expect(migrateUIState({ theme: 'light', namespace: 'team-a' }, 0)).toEqual({ theme: 'system', namespace: 'team-a' })
    expect(migrateUIState({ theme: 'dark' }, 0)).toEqual({ theme: 'dark' })
    expect(migrateUIState({ theme: 'light' }, 1)).toEqual({ theme: 'light' })
    expect(migrateUIState(undefined, 0)).toEqual({})
  })
})

describe('mobileSidebarOpen', () => {
  it('is not part of the persisted state', () => {
    useUIStore.setState({ mobileSidebarOpen: true })
    expect(Object.keys(persistedUIState(useUIStore.getState()))).toEqual(['sidebarCollapsed', 'theme', 'namespace'])
  })
})
