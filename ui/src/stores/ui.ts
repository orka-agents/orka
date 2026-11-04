import { create } from 'zustand'
import { persist } from 'zustand/middleware'

/** The theme the user picked; 'system' follows the operating system's setting. */
export type ThemePreference = 'system' | 'light' | 'dark'

interface UIState {
  // Persisted desktop preference.
  sidebarCollapsed: boolean
  // Responsive overlay state below the md breakpoint; never persisted so a
  // narrow viewport cannot overwrite the desktop preference.
  mobileSidebarOpen: boolean
  theme: ThemePreference
  namespace: string
  toggleSidebar: () => void
  setSidebarCollapsed: (collapsed: boolean) => void
  setMobileSidebarOpen: (open: boolean) => void
  setTheme: (theme: ThemePreference) => void
  setNamespace: (namespace: string) => void
}

// Only durable preferences are persisted; responsive state such as the
// mobile sidebar overlay is session-local.
export const persistedUIState = (state: UIState) => ({
  sidebarCollapsed: state.sidebarCollapsed,
  theme: state.theme,
  namespace: state.namespace,
})

/**
 * Version 1 added 'system' and made it the default. Before that every visitor
 * started on 'light', so a stored 'light' was almost never a choice and now
 * follows the system. index.html applies the same rule before first paint.
 */
export function migrateUIState(persisted: unknown, version: number) {
  const state = (persisted ?? {}) as Partial<ReturnType<typeof persistedUIState>>
  if (version < 1 && state.theme === 'light') return { ...state, theme: 'system' as const }
  return state
}

export const useUIStore = create<UIState>()(
  persist(
    (set, get) => ({
      sidebarCollapsed: false,
      mobileSidebarOpen: false,
      theme: 'system',
      namespace: 'orka-system',
      toggleSidebar: () => set({ sidebarCollapsed: !get().sidebarCollapsed }),
      setSidebarCollapsed: (sidebarCollapsed) => set({ sidebarCollapsed }),
      setMobileSidebarOpen: (mobileSidebarOpen) => set({ mobileSidebarOpen }),
      setTheme: (theme) => set({ theme }),
      setNamespace: (namespace) => set({ namespace }),
    }),
    {
      name: 'orka-ui',
      version: 1,
      migrate: migrateUIState,
      partialize: persistedUIState,
    }
  )
)
