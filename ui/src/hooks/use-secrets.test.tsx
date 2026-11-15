import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { isForbiddenError } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import { useSecretNames } from './use-secrets'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useSecretNames', () => {
  it('returns secret names from API', async () => {
    const { result } = renderHook(() => useSecretNames(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [] })
  })

  it('reports a forbidden list at once instead of retrying it', async () => {
    let requests = 0
    server.use(http.get('/api/v1/secrets', () => {
      requests += 1
      return HttpResponse.json({ error: { code: 403, message: 'forbidden' } }, { status: 403 })
    }))
    const { result } = renderHook(() => useSecretNames(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isError).toBe(true))
    expect(isForbiddenError(result.current.error)).toBe(true)
    expect(requests).toBe(1)
  })
})
