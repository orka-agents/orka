import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { http, HttpResponse } from 'msw'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import {
  useCreateProvider,
  useDeleteProvider,
  useProvider,
  useProviderList,
  useUpdateProvider,
} from './use-providers'

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

describe('useProviderList', () => {
  it('lists providers for the selected namespace', async () => {
    server.use(
      http.get('/api/v1/providers', ({ request }) => {
        const url = new URL(request.url)
        expect(url.searchParams.get('namespace')).toBe('default')
        return HttpResponse.json({
          items: [{ name: 'anthropic', namespace: 'default', type: 'anthropic', defaultModel: 'claude', ready: true }],
          metadata: {},
        })
      }),
    )
    const { result } = renderHook(() => useProviderList(false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.items).toHaveLength(1)
    expect(result.current.data?.items[0].name).toBe('anthropic')
  })
})

describe('useProvider', () => {
  it('fetches a full provider object', async () => {
    const { result } = renderHook(() => useProvider('anthropic'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.spec.type).toBe('anthropic')
  })
})

describe('provider mutations', () => {
  it('creates a provider', async () => {
    const { result } = renderHook(() => useCreateProvider(), { wrapper: createWrapper() })
    result.current.mutate({
      name: 'openai',
      namespace: 'default',
      spec: { type: 'openai', secretRef: { name: 'openai-key' } },
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })

  it('updates a provider spec', async () => {
    const { result } = renderHook(() => useUpdateProvider(), { wrapper: createWrapper() })
    result.current.mutate({
      name: 'anthropic',
      spec: { type: 'anthropic', secretRef: { name: 'rotated' } },
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })

  it('deletes a provider', async () => {
    const { result } = renderHook(() => useDeleteProvider(), { wrapper: createWrapper() })
    result.current.mutate('anthropic')
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
