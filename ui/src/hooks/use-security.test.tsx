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
import { useAllFindings, useRunSecurityScan } from './use-security'

function createWrapper(queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })) {
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

describe('useRunSecurityScan', () => {
  it('starts scans with namespace as a query parameter and invalidates repository lists', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
    })
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
    useUIStore.setState({ namespace: 'qa-ns', sidebarCollapsed: false, theme: 'light' })
    let requestURL: URL | undefined
    let receivedBody: unknown

    server.use(
      http.post('/api/v1/security/repositories/:name/scans', async ({ request }) => {
        requestURL = new URL(request.url)
        receivedBody = await request.json()
        return HttpResponse.json({
          id: 'scan-1',
          namespace: 'qa-ns',
          repositoryScan: 'demo-repo',
          taskName: 'scan-task',
          mode: 'manual',
          phase: 'queued',
          startedAt: '2026-01-01T00:00:00Z',
        })
      }),
    )

    const { result } = renderHook(() => useRunSecurityScan('demo-repo'), {
      wrapper: createWrapper(queryClient),
    })

    await act(async () => {
      await result.current.mutateAsync()
    })

    expect(requestURL?.searchParams.get('namespace')).toBe('qa-ns')
    expect(receivedBody).toEqual({})
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['security', 'scans', 'qa-ns', 'demo-repo'] })
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['security', 'repository', 'qa-ns', 'demo-repo'] })
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['security', 'repositories', 'qa-ns'] })
  })
})
