import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createHashHistory, createRouter } from '@tanstack/react-router'
import { routeTree } from './routeTree.gen'
import { NotFoundPage } from './components/common/not-found-page'
import { captureHashToken } from './lib/login-return'
import { useAuthStore } from './stores/auth'
import { useChatStore } from './stores/chat'
import { useUIStore } from './stores/ui'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30 * 1000,
      retry: 1,
    },
  },
})

let lastToken = useAuthStore.getState().token
useAuthStore.subscribe((state) => {
  if (state.token !== lastToken) {
    lastToken = state.token
    queryClient.clear()
    useChatStore.getState().resetForIdentityChange()
  }
})

let lastNamespace = useUIStore.getState().namespace
useChatStore.getState().selectNamespace(lastNamespace)
useUIStore.subscribe((state) => {
  if (state.namespace !== lastNamespace) {
    lastNamespace = state.namespace
    useChatStore.getState().selectNamespace(state.namespace)
  }
})

captureHashToken()

const router = createRouter({
  routeTree,
  defaultNotFoundComponent: NotFoundPage,
  ...(import.meta.env.MODE === 'demo' && { history: createHashHistory() }),
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}
