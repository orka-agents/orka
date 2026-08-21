import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { describe, expect, it, beforeEach, vi } from 'vitest'
import { server } from '@/test/mocks/server'
import type { SecurityFinding } from '@/schemas/security'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useAllFindings, useRepositoryScans, useRunSecurityScan } from './use-security'

function createQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
}

function createWrapper(queryClient = createQueryClient()) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function makeFinding(id: string): SecurityFinding {
  return {
    id,
    namespace: 'default',
    repositoryScan: 'repo',
    fingerprint: `fingerprint-${id}`,
    title: `Finding ${id}`,
    summary: `Summary ${id}`,
    severity: 'low',
    confidence: 'medium',
    validationStatus: 'unknown',
    state: 'open',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('useAllFindings', () => {
  it('fetches all findings pages using continuation cursors', async () => {
    const requests: URL[] = []

    server.use(
      http.get('/api/v1/security/repositories/:name/findings', ({ request }) => {
        const url = new URL(request.url)
        requests.push(url)

        if (!url.searchParams.has('cursor')) {
          return HttpResponse.json({
            items: [makeFinding('finding-1')],
            metadata: { continue: '1' },
          })
        }

        return HttpResponse.json({
          items: [makeFinding('finding-2')],
          metadata: {},
        })
      }),
    )

    const { result } = renderHook(() => useAllFindings('repo'), { wrapper: createWrapper() })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(result.current.data?.items.map((finding) => finding.id)).toEqual(['finding-1', 'finding-2'])
    expect(requests).toHaveLength(2)
    expect(requests[0].searchParams.get('limit')).toBe('100')
    expect(requests[0].searchParams.has('cursor')).toBe(false)
    expect(requests[1].searchParams.get('limit')).toBe('100')
    expect(requests[1].searchParams.get('cursor')).toBe('1')
  })
})


describe('repository scan list batching', () => {
  it('requests repositories and latest runs in one response', async () => {
    const requests: URL[] = []
    server.use(
      http.get('/api/v1/security/repositories', ({ request }) => {
        requests.push(new URL(request.url))
        return HttpResponse.json({ items: [], latestScanRuns: [], metadata: {} })
      }),
    )

    const { result } = renderHook(() => useRepositoryScans(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(requests).toHaveLength(1)
    expect(requests[0].searchParams.get('namespace')).toBe('default')
    expect(requests[0].searchParams.get('includeLatestRuns')).toBe('true')
    expect(result.current.data?.latestScanRuns).toEqual([])
  })

  it('invalidates the combined repository snapshot after starting a scan', async () => {
    server.use(
      http.post('/api/v1/security/repositories/:name/scans', () =>
        HttpResponse.json({ id: 'scan-new', namespace: 'default', repositoryScan: 'repo' }),
      ),
    )
    const queryClient = createQueryClient()
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
    const { result } = renderHook(() => useRunSecurityScan('repo'), {
      wrapper: createWrapper(queryClient),
    })

    act(() => result.current.mutate())
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(invalidateSpy).toHaveBeenCalledWith({
      queryKey: ['security', 'repositories', 'default'],
    })
  })
})
