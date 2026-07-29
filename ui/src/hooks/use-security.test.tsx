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
import {
  useAllFindings,
  useCreatePullRequest,
  useDismissFinding,
  useGeneratePatch,
  useReopenFinding,
  useRunSecurityScan,
  useValidateFinding,
} from './use-security'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
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

interface CapturedRequest {
  body: string
  url: URL
}

function captureSecurityMutation(path: string) {
  const requests: CapturedRequest[] = []

  server.use(
    http.post(path, async ({ request }) => {
      requests.push({
        body: await request.text(),
        url: new URL(request.url),
      })
      return HttpResponse.json({})
    }),
  )

  return requests
}

function expectNamespaceQuery(requests: CapturedRequest[]) {
  expect(requests).toHaveLength(1)
  expect(requests[0].url.searchParams.get('namespace')).toBe('team-blue')
  expect(requests[0].body).toBe('')
}

describe('security mutations', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'team-blue' })
  })

  it('runs a repository scan with the selected namespace in the query', async () => {
    const requests = captureSecurityMutation('/api/v1/security/repositories/repo/scans')
    const { result } = renderHook(() => useRunSecurityScan('repo'), { wrapper: createWrapper() })

    await act(async () => {
      await result.current.mutateAsync()
    })

    expectNamespaceQuery(requests)
  })

  it('dismisses a finding with the selected namespace in the query', async () => {
    const requests = captureSecurityMutation('/api/v1/security/findings/finding-1/dismiss')
    const { result } = renderHook(() => useDismissFinding('finding-1'), { wrapper: createWrapper() })

    await act(async () => {
      await result.current.mutateAsync()
    })

    expectNamespaceQuery(requests)
  })

  it('reopens a finding with the selected namespace in the query', async () => {
    const requests = captureSecurityMutation('/api/v1/security/findings/finding-1/reopen')
    const { result } = renderHook(() => useReopenFinding('finding-1'), { wrapper: createWrapper() })

    await act(async () => {
      await result.current.mutateAsync()
    })

    expectNamespaceQuery(requests)
  })

  it('generates a patch with the selected namespace in the query', async () => {
    const requests = captureSecurityMutation('/api/v1/security/findings/finding-1/patch')
    const { result } = renderHook(() => useGeneratePatch('finding-1'), { wrapper: createWrapper() })

    await act(async () => {
      await result.current.mutateAsync()
    })

    expectNamespaceQuery(requests)
  })

  it('validates a finding with the selected namespace in the query', async () => {
    const requests = captureSecurityMutation('/api/v1/security/findings/finding-1/validate')
    const { result } = renderHook(() => useValidateFinding('finding-1'), { wrapper: createWrapper() })

    await act(async () => {
      await result.current.mutateAsync()
    })

    expectNamespaceQuery(requests)
  })

  it('creates a pull request with the selected namespace in the query', async () => {
    const requests = captureSecurityMutation('/api/v1/security/findings/finding-1/pull-request')
    const { result } = renderHook(() => useCreatePullRequest('finding-1'), { wrapper: createWrapper() })

    await act(async () => {
      await result.current.mutateAsync()
    })

    expectNamespaceQuery(requests)
  })
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
