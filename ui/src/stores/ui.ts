import { create } from 'zustand'
import { persist } from 'zustand/middleware'

const NAMESPACE_HISTORY_LIMIT = 8

interface UIState {
  sidebarCollapsed: boolean
  theme: 'light' | 'dark'
  namespace: string
  // Recently used namespaces, most recent first. There is no list-namespaces
  // API, so the switcher is a free-text field backed by this history.
  namespaceHistory: string[]
  toggleSidebar: () => void
  toggleTheme: () => void
  setNamespace: (namespace: string) => void
}

export const useUIStore = create<UIState>()(
  persist(
    (set, get) => ({
      sidebarCollapsed: false,
      theme: 'light',
      namespace: 'orka-system',
      namespaceHistory: [],
      toggleSidebar: () => set({ sidebarCollapsed: !get().sidebarCollapsed }),
      toggleTheme: () => {
        const newTheme = get().theme === 'light' ? 'dark' : 'light'
        document.documentElement.classList.toggle('dark', newTheme === 'dark')
        set({ theme: newTheme })
      },
      setNamespace: (namespace) => {
        const trimmed = namespace.trim()
        if (!trimmed) return
        const history = [
          trimmed,
          ...(get().namespaceHistory ?? []).filter((entry) => entry !== trimmed),
        ].slice(0, NAMESPACE_HISTORY_LIMIT)
        set({ namespace: trimmed, namespaceHistory: history })
      },
    }),
    { name: 'orka-ui' }
  )
)
