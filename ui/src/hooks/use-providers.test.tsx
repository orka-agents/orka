import { describe, it, expect, beforeEach, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useProviderList } from './use-providers'

function createWrapper() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

describe('useProviderList', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'team-a', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('lists providers for the selected namespace, normalized', async () => {
    let requestedNamespace: string | null = null
    server.use(
      http.get('/api/v1/providers', ({ request }) => {
        requestedNamespace = new URL(request.url).searchParams.get('namespace')
        return HttpResponse.json({
          items: [{ metadata: { name: 'anthropic', namespace: 'team-a' }, spec: { type: 'anthropic', defaultModel: 'claude-sonnet-4-20250514' } }],
          metadata: {},
        })
      }),
    )
    const { result } = renderHook(() => useProviderList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(requestedNamespace).toBe('team-a')
    expect(result.current.data?.items[0]).toMatchObject({ name: 'anthropic', defaultModel: 'claude-sonnet-4-20250514' })
  })
})

describe('useProviderList pagination', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'team-a', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('follows metadata.continue so providers beyond the first page are listed once', async () => {
    const cursors: Array<string | null> = []
    server.use(
      http.get('/api/v1/providers', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('continue')
        cursors.push(cursor)
        if (!cursor) {
          return HttpResponse.json({
            items: [{ metadata: { name: 'p1', namespace: 'team-a' }, spec: { type: 'openai' } }],
            metadata: { continue: 'page-2' },
          })
        }
        return HttpResponse.json({
          items: [{ metadata: { name: 'p2', namespace: 'team-a' }, spec: { type: 'anthropic' } }],
          metadata: {},
        })
      }),
    )
    const { result } = renderHook(() => useProviderList(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(cursors).toEqual([null, 'page-2'])
    expect(result.current.data?.items.map((p) => p.name)).toEqual(['p1', 'p2'])
  })
})
