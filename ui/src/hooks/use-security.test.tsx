import { act, renderHook, waitFor } from '@testing-library/react'
import { onlineManager, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest'
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
  useFinding,
  useGeneratePatch,
  useReopenFinding,
  useRepositoryScan,
  useRunSecurityScan,
  useUpdateThreatModel,
  useValidateFinding,
} from './use-security'

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

  it('saves a threat model to the namespace it was saved in, and files it there', async () => {
    let putNamespace: string | null = null
    server.use(http.put('/api/v1/security/repositories/:name/threat-model', ({ request, params }) => {
      putNamespace = new URL(request.url).searchParams.get('namespace')
      return HttpResponse.json({ namespace: 'team-blue', repositoryScan: params.name, version: 2, content: 'saved', source: 'edited' })
    }))
    const queryClient = createQueryClient()
    const { result } = renderHook(() => useUpdateThreatModel('repo'), { wrapper: createWrapper(queryClient) })

    // Offline, the save waits before sending; the user switches namespace meanwhile.
    onlineManager.setOnline(false)
    try {
      let save: Promise<unknown> = Promise.resolve()
      act(() => {
        save = result.current.mutateAsync({ content: 'saved', expectedVersion: 1 })
      })
      act(() => useUIStore.setState({ namespace: 'team-red' }))
      await act(async () => {
        onlineManager.setOnline(true)
        await save
      })
    } finally {
      onlineManager.setOnline(true)
    }

    expect(putNamespace).toBe('team-blue')
    expect(queryClient.getQueryData(['security', 'threat-model', 'team-blue', 'repo'])).toMatchObject({ version: 2 })
    expect(queryClient.getQueryData(['security', 'threat-model', 'team-red', 'repo'])).toBeUndefined()
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

  it('invalidates the scanned repository even when route and namespace change before success', async () => {
    let releaseRequest: (() => void) | undefined
    let requestURL: URL | undefined

    server.use(
      http.post('/api/v1/security/repositories/:name/scans', async ({ request }) => {
        requestURL = new URL(request.url)
        await new Promise<void>((resolve) => {
          releaseRequest = () => resolve()
        })
        return HttpResponse.json({})
      }),
    )

    const queryClient = createQueryClient()
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
    const { result, rerender } = renderHook(
      ({ name }) => useRunSecurityScan(name),
      { initialProps: { name: 'repo-1' }, wrapper: createWrapper(queryClient) },
    )
    let mutationPromise: Promise<unknown>

    act(() => {
      mutationPromise = result.current.mutateAsync()
    })

    await waitFor(() => expect(requestURL?.pathname).toBe('/api/v1/security/repositories/repo-1/scans'))
    expect(requestURL?.searchParams.get('namespace')).toBe('team-blue')

    act(() => {
      useUIStore.setState({ namespace: 'team-red' })
    })
    rerender({ name: 'repo-2' })

    await act(async () => {
      releaseRequest?.()
      await mutationPromise
    })

    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['security', 'scans', 'team-blue', 'repo-1'] })
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['security', 'repository', 'team-blue', 'repo-1'] })
    expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'scans', 'team-red', 'repo-2'] })
    expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'repository', 'team-red', 'repo-2'] })
  })

  it('invalidates the finding mutation target even when route and namespace change before success', async () => {
    const paths = [
      '/api/v1/security/findings/finding-1/dismiss',
      '/api/v1/security/findings/finding-1/reopen',
      '/api/v1/security/findings/finding-1/patch',
      '/api/v1/security/findings/finding-1/validate',
      '/api/v1/security/findings/finding-1/pull-request',
    ]
    let releaseRequest: (() => void) | undefined
    let startedPath: string | undefined
    let requestNamespace: string | null = null

    for (const path of paths) {
      server.use(
        http.post(path, async ({ request }) => {
          startedPath = new URL(request.url).pathname
          requestNamespace = new URL(request.url).searchParams.get('namespace')
          await new Promise<void>((resolve) => {
            releaseRequest = () => resolve()
          })
          return HttpResponse.json({})
        }),
      )
    }

    const queryClient = createQueryClient()
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
    const { result, rerender } = renderHook(({ id }) => ({
      dismiss: useDismissFinding(id),
      reopen: useReopenFinding(id),
      patch: useGeneratePatch(id),
      validate: useValidateFinding(id),
      pullRequest: useCreatePullRequest(id),
    }), { initialProps: { id: 'finding-1' }, wrapper: createWrapper(queryClient) })

    const findingKey = ['security', 'finding', 'team-blue', 'finding-1']
    const findingsKey = ['security', 'findings', 'team-blue']
    const repositoriesKey = ['security', 'repositories', 'team-blue']
    const patchesKey = ['security', 'patches', 'team-blue', 'finding-1']
    const cases = [
      {
        path: paths[0],
        mutate: () => result.current.dismiss.mutateAsync(),
        expectedKeys: [findingKey, findingsKey, repositoriesKey],
      },
      {
        path: paths[1],
        mutate: () => result.current.reopen.mutateAsync(),
        expectedKeys: [findingKey, findingsKey, repositoriesKey],
      },
      {
        path: paths[2],
        mutate: () => result.current.patch.mutateAsync(),
        expectedKeys: [findingKey, patchesKey, findingsKey],
      },
      {
        path: paths[3],
        mutate: () => result.current.validate.mutateAsync(),
        expectedKeys: [findingKey, findingsKey, repositoriesKey],
      },
      {
        path: paths[4],
        mutate: () => result.current.pullRequest.mutateAsync(),
        expectedKeys: [findingKey, patchesKey, findingsKey],
      },
    ]

    for (const mutationCase of cases) {
      act(() => {
        useUIStore.setState({ namespace: 'team-blue' })
      })
      rerender({ id: 'finding-1' })
      invalidateSpy.mockClear()
      releaseRequest = undefined
      startedPath = undefined
      requestNamespace = null
      let mutationPromise: Promise<unknown>

      act(() => {
        mutationPromise = mutationCase.mutate()
      })

      await waitFor(() => expect(startedPath).toBe(mutationCase.path))
      expect(requestNamespace).toBe('team-blue')

      act(() => {
        useUIStore.setState({ namespace: 'team-red' })
      })
      rerender({ id: 'finding-2' })

      await act(async () => {
        releaseRequest?.()
        await mutationPromise
      })

      expect(invalidateSpy).toHaveBeenCalledTimes(mutationCase.expectedKeys.length)
      for (const queryKey of mutationCase.expectedKeys) {
        expect(invalidateSpy).toHaveBeenCalledWith({ queryKey })
      }
      expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'findings'] })
      expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'repositories'] })
      expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'findings', 'team-red'] })
      expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'repositories', 'team-red'] })
      expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'finding', 'team-blue', 'finding-2'] })
      expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: ['security', 'patches', 'team-blue', 'finding-2'] })
    }
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

    const queryClient = createQueryClient()
    const { result } = renderHook(() => useAllFindings('repo'), { wrapper: createWrapper(queryClient) })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(result.current.data?.items.map((finding) => finding.id)).toEqual(['finding-1', 'finding-2'])
    expect(requests).toHaveLength(2)
    expect(requests[0].searchParams.get('limit')).toBe('100')
    expect(requests[0].searchParams.has('cursor')).toBe(false)
    expect(requests[1].searchParams.get('limit')).toBe('100')
    expect(requests[1].searchParams.get('cursor')).toBe('1')
    expect(queryClient.getQueryCache().findAll({
      queryKey: ['security', 'findings', 'default'],
    })).toHaveLength(1)
  })
})

describe('security detail reads on 404', () => {
  const notFound = () => HttpResponse.json({ error: { code: 404, message: 'not found' } }, { status: 404 })

  async function advance(ms: number) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms)
    })
  }

  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('does not retry a missing repository, but keeps polling one never seen', async () => {
    let calls = 0
    server.use(http.get('/api/v1/security/repositories/new-repo', () => {
      calls += 1
      return notFound()
    }))

    // The app's default of one retry, which the hook must override for a 404.
    const appClient = new QueryClient({ defaultOptions: { queries: { retry: 1, gcTime: 0 } } })
    renderHook(() => useRepositoryScan('new-repo'), { wrapper: createWrapper(appClient) })

    await advance(9_000)
    expect(calls).toBe(1)
    // A just-created RepositoryScan can 404 until the API cache catches up.
    await advance(1_500)
    expect(calls).toBe(2)
  })

  it('stops polling a repository that 404s after it loaded', async () => {
    let calls = 0
    server.use(http.get('/api/v1/security/repositories/deleted-repo', () => {
      calls += 1
      return calls === 1
        ? HttpResponse.json({ metadata: { name: 'deleted-repo' }, spec: { repoURL: 'https://github.com/org/repo', analysisAgentRef: { name: 'agent' } } })
        : notFound()
    }))

    renderHook(() => useRepositoryScan('deleted-repo'), { wrapper: createWrapper() })

    await advance(10_500)
    expect(calls).toBe(2)
    await advance(60_000)
    expect(calls).toBe(2)
  })

  it('neither retries nor polls a missing finding', async () => {
    let calls = 0
    server.use(http.get('/api/v1/security/findings/missing', () => {
      calls += 1
      return notFound()
    }))

    const appClient = new QueryClient({ defaultOptions: { queries: { retry: 1, gcTime: 0 } } })
    renderHook(() => useFinding('missing'), { wrapper: createWrapper(appClient) })

    await advance(60_000)
    expect(calls).toBe(1)
  })
})
