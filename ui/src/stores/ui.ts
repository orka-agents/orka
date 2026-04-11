import { create } from 'zustand'
import { persist } from 'zustand/middleware'

interface UIState {
  sidebarCollapsed: boolean
  theme: 'light' | 'dark'
  namespace: string
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
      toggleSidebar: () => set({ sidebarCollapsed: !get().sidebarCollapsed }),
      toggleTheme: () => {
        const newTheme = get().theme === 'light' ? 'dark' : 'light'
        document.documentElement.classList.toggle('dark', newTheme === 'dark')
        set({ theme: newTheme })
      },
      setNamespace: (namespace) => set({ namespace }),
    }),
    { name: 'orka-ui' }
  )
)
