import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { http, HttpResponse } from 'msw'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { ApiError } from '@/lib/api-client'
import {
  isMemoryStoreUnavailable,
  useApplyMemoryProposal,
  useArchiveMemoryProposal,
  useCreateMemory,
  useDeleteMemory,
  useMemory,
  useMemoryList,
  useMemoryProposal,
  useMemoryProposalList,
  useReviewMemoryProposal,
  useSetMemoryEnabled,
  useUpdateMemory,
} from './use-memory'

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

describe('useMemoryList', () => {
  it('passes filters as query params', async () => {
    server.use(
      http.get('/api/v1/memories', ({ request }) => {
        const url = new URL(request.url)
        expect(url.searchParams.get('namespace')).toBe('default')
        expect(url.searchParams.get('query')).toBe('deploy')
        expect(url.searchParams.get('tags')).toBe('infra,ci')
        expect(url.searchParams.get('includeDisabled')).toBe('true')
        return HttpResponse.json({
          items: [
            {
              id: 'm1',
              namespace: 'default',
              source: 'manual',
              content: 'deploy uses kind',
              createdAt: '2026-06-13T00:00:00Z',
              updatedAt: '2026-06-13T00:00:00Z',
            },
          ],
          metadata: {},
        })
      }),
    )
    const { result } = renderHook(
      () => useMemoryList({ query: 'deploy', tags: ['infra', 'ci'], includeDisabled: true }, false),
      { wrapper: createWrapper() },
    )
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.items[0].id).toBe('m1')
  })

  it('treats 501 as memory store unavailable', async () => {
    server.use(
      http.get('/api/v1/memories', () =>
        HttpResponse.json({ error: { code: 501, message: 'memory store not configured' } }, { status: 501 }),
      ),
    )
    const { result } = renderHook(() => useMemoryList({}, false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isError).toBe(true))
    expect(result.current.error).toBeInstanceOf(ApiError)
    expect(isMemoryStoreUnavailable(result.current.error)).toBe(true)
  })
})

describe('memory detail + mutations', () => {
  it('fetches one memory', async () => {
    const { result } = renderHook(() => useMemory('m1'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.content).toBe('remembered fact')
  })

  it('creates, updates, toggles, and deletes memories', async () => {
    const wrapper = createWrapper()
    const create = renderHook(() => useCreateMemory(), { wrapper })
    create.result.current.mutate({ content: 'new memory' })
    await waitFor(() => expect(create.result.current.isSuccess).toBe(true))

    const update = renderHook(() => useUpdateMemory(), { wrapper })
    update.result.current.mutate({ id: 'm1', content: 'updated' })
    await waitFor(() => expect(update.result.current.isSuccess).toBe(true))

    const toggle = renderHook(() => useSetMemoryEnabled(), { wrapper })
    toggle.result.current.mutate({ id: 'm1', enabled: false })
    await waitFor(() => expect(toggle.result.current.isSuccess).toBe(true))

    const del = renderHook(() => useDeleteMemory(), { wrapper })
    del.result.current.mutate('m1')
    await waitFor(() => expect(del.result.current.isSuccess).toBe(true))
  })
})

describe('memory proposals', () => {
  it('lists proposals with status filter', async () => {
    server.use(
      http.get('/api/v1/memory-proposals', ({ request }) => {
        const url = new URL(request.url)
        expect(url.searchParams.get('status')).toBe('pending')
        return HttpResponse.json({
          items: [
            {
              id: 'p1',
              namespace: 'default',
              type: 'memory',
              title: 'Remember the deploy flow',
              status: 'pending',
              createdAt: '2026-06-13T00:00:00Z',
              updatedAt: '2026-06-13T00:00:00Z',
            },
          ],
          metadata: {},
        })
      }),
    )
    const { result } = renderHook(() => useMemoryProposalList({ status: 'pending' }, false), {
      wrapper: createWrapper(),
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.items[0].title).toBe('Remember the deploy flow')
  })

  it('fetches proposal detail', async () => {
    const { result } = renderHook(() => useMemoryProposal('p1'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.status).toBe('pending')
  })

  it('reviews with a decision body', async () => {
    let sawBody: unknown
    server.use(
      http.post('/api/v1/memory-proposals/:id/review', async ({ request }) => {
        sawBody = await request.json()
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const { result } = renderHook(() => useReviewMemoryProposal(), { wrapper: createWrapper() })
    result.current.mutate({ id: 'p1', status: 'accepted', reviewNote: 'looks right' })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(sawBody).toEqual({ status: 'accepted', reviewNote: 'looks right' })
  })

  it('applies a proposal and returns the created memory', async () => {
    const { result } = renderHook(() => useApplyMemoryProposal(), { wrapper: createWrapper() })
    result.current.mutate({ id: 'p1' })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.sourceProposalId).toBe('p1')
  })

  it('archives a proposal', async () => {
    const { result } = renderHook(() => useArchiveMemoryProposal(), { wrapper: createWrapper() })
    result.current.mutate('p1')
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
