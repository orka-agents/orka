import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useSessionList, useSession, useDeleteSession } from './use-sessions'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useSessionList', () => {
  it('returns session list from API', async () => {
    const { result } = renderHook(() => useSessionList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual({ items: [], metadata: {} })
  })
})

describe('useSession', () => {
  it('returns a single session by id', async () => {
    const { result } = renderHook(() => useSession('sess-1'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({
      name: 'sess-1',
      namespace: 'default',
      messageCount: '5',
    })
  })
})

describe('useDeleteSession', () => {
  it('deletes a session via mutation', async () => {
    const { result } = renderHook(() => useDeleteSession(), { wrapper: createWrapper() })
    act(() => {
      result.current.mutate('sess-1')
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
