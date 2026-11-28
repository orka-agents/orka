import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import { isKubernetesNamespace } from '@/lib/connectors'

export type ThemePreference = 'system' | 'light' | 'dark'

export const RECENT_NAMESPACE_LIMIT = 10

interface UIState {
  sidebarCollapsed: boolean
  mobileSidebarOpen: boolean
  theme: ThemePreference
  namespace: string
  recentNamespaces: string[]
  callerNamespace: string | null
  toggleSidebar: () => void
  setMobileSidebarOpen: (open: boolean) => void
  setTheme: (theme: ThemePreference) => void
  setNamespace: (namespace: string) => void
  adoptCallerNamespace: (namespace: string | undefined) => void
}

export const persistedUIState = (state: UIState) => ({
  sidebarCollapsed: state.sidebarCollapsed,
  theme: state.theme,
  namespace: state.namespace,
  recentNamespaces: state.recentNamespaces,
  callerNamespace: state.callerNamespace,
})

function rememberNamespace(recent: string[], namespace: string) {
  return [namespace, ...recent.filter((entry) => entry !== namespace)].slice(0, RECENT_NAMESPACE_LIMIT)
}

export function namespaceOptions(state: Pick<UIState, 'namespace' | 'recentNamespaces' | 'callerNamespace'>) {
  const options = new Set([state.callerNamespace ?? undefined, state.namespace, ...state.recentNamespaces])
  return [...options].filter(isKubernetesNamespace).sort()
}

export function migrateUIState(persisted: unknown, version: number) {
  let state = (persisted ?? {}) as Partial<ReturnType<typeof persistedUIState>>
  if (version < 1 && state.theme === 'light') state = { ...state, theme: 'system' as const }
  if (version < 2 && isKubernetesNamespace(state.namespace)) state = { ...state, recentNamespaces: [state.namespace] }
  return state
}

export const useUIStore = create<UIState>()(
  persist(
    (set, get) => ({
      sidebarCollapsed: false,
      mobileSidebarOpen: false,
      theme: 'system',
      namespace: 'orka-system',
      recentNamespaces: [],
      callerNamespace: null,
      toggleSidebar: () => set({ sidebarCollapsed: !get().sidebarCollapsed }),
      setMobileSidebarOpen: (mobileSidebarOpen) => set({ mobileSidebarOpen }),
      setTheme: (theme) => set({ theme }),
      setNamespace: (namespace) => {
        if (!isKubernetesNamespace(namespace)) return
        set({ namespace, recentNamespaces: rememberNamespace(get().recentNamespaces, namespace) })
      },
      adoptCallerNamespace: (namespace) => {
        if (!isKubernetesNamespace(namespace)) {
          if (get().callerNamespace !== null) set({ callerNamespace: null })
          return
        }
        if (namespace === get().callerNamespace) return
        set({
          namespace,
          callerNamespace: namespace,
          recentNamespaces: rememberNamespace(get().recentNamespaces, namespace),
        })
      },
    }),
    {
      name: 'orka-ui',
      version: 2,
      migrate: migrateUIState,
      partialize: persistedUIState,
    }
  )
)
