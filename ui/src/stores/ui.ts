import { create } from 'zustand'
import { persist } from 'zustand/middleware'

export type ThemePreference = 'system' | 'light' | 'dark'

interface UIState {
  sidebarCollapsed: boolean
  mobileSidebarOpen: boolean
  theme: ThemePreference
  namespace: string
  toggleSidebar: () => void
  setMobileSidebarOpen: (open: boolean) => void
  setTheme: (theme: ThemePreference) => void
  setNamespace: (namespace: string) => void
}

export const persistedUIState = (state: UIState) => ({
  sidebarCollapsed: state.sidebarCollapsed,
  theme: state.theme,
  namespace: state.namespace,
})

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
