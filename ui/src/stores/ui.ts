import { create } from 'zustand'
import { persist } from 'zustand/middleware'

interface UIState {
  // Persisted desktop preference.
  sidebarCollapsed: boolean
  // Responsive overlay state below the md breakpoint; never persisted so a
  // narrow viewport cannot overwrite the desktop preference.
  mobileSidebarOpen: boolean
  theme: 'light' | 'dark'
  namespace: string
  toggleSidebar: () => void
  setSidebarCollapsed: (collapsed: boolean) => void
  setMobileSidebarOpen: (open: boolean) => void
  toggleTheme: () => void
  setNamespace: (namespace: string) => void
}

// Only durable preferences are persisted; responsive state such as the
// mobile sidebar overlay is session-local.
export const persistedUIState = (state: UIState) => ({
  sidebarCollapsed: state.sidebarCollapsed,
  theme: state.theme,
  namespace: state.namespace,
})

export const useUIStore = create<UIState>()(
  persist(
    (set, get) => ({
      sidebarCollapsed: false,
      mobileSidebarOpen: false,
      theme: 'light',
      namespace: 'orka-system',
      toggleSidebar: () => set({ sidebarCollapsed: !get().sidebarCollapsed }),
      setSidebarCollapsed: (sidebarCollapsed) => set({ sidebarCollapsed }),
      setMobileSidebarOpen: (mobileSidebarOpen) => set({ mobileSidebarOpen }),
      toggleTheme: () => {
        const newTheme = get().theme === 'light' ? 'dark' : 'light'
        document.documentElement.classList.toggle('dark', newTheme === 'dark')
        set({ theme: newTheme })
      },
      setNamespace: (namespace) => set({ namespace }),
    }),
    {
      name: 'orka-ui',
      partialize: persistedUIState,
    }
  )
)
