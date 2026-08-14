import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { http, HttpResponse } from 'msw'

import { server } from '@/test/mocks/server'
import { useWhoAmI } from './use-whoami'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

describe('useWhoAmI', () => {
  it('returns the verified identity', async () => {
    const { result } = renderHook(() => useWhoAmI(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.authenticated).toBe(true)
    expect(result.current.data?.username).toContain('serviceaccount')
  })

  it('exposes transaction metadata when present', async () => {
    server.use(
      http.get('/api/v1/auth/whoami', () =>
        HttpResponse.json({
          authenticated: true,
          authType: 'context-token',
          subject: 'workload-a',
          transaction: { profile: 'transaction-token', id: 'tx-1', scopes: ['orka:tasks:create'] },
        }),
      ),
    )
    const { result } = renderHook(() => useWhoAmI(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.transaction?.id).toBe('tx-1')
  })
})
