// Tests for the parity additions layered onto existing hook modules:
// agent update, tool CRUD, external runtime registration, substrate pools,
// monitor update/events/patch preview, and security scan update/delete/slice.
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { http, HttpResponse } from 'msw'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { useUpdateAgent } from './use-agents'
import { useCreateTool, useDeleteTool, useUpdateTool } from './use-tools'
import {
  useCreateAgentRuntime,
  useCreateSubstrateActorPool,
  useDeleteAgentRuntime,
  useDeleteSubstrateActorPool,
  useSubstrateActorPool,
  useSubstrateActorPools,
  useUpdateAgentRuntime,
  useUpdateSubstrateActorPool,
} from './use-runtimes'
import {
  useImplementationJobPatchPreview,
  useRepositoryMonitorEvents,
  useUpdateRepositoryMonitor,
} from './use-monitors'
import {
  useDeleteRepositoryScan,
  useReviewSliceDetail,
  useUpdateRepositoryScan,
} from './use-security'

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

describe('useUpdateAgent', () => {
  it('PUTs the agent spec', async () => {
    let sawBody: unknown
    server.use(
      http.put('/api/v1/agents/:name', async ({ request }) => {
        sawBody = await request.json()
        return HttpResponse.json({ metadata: { name: 'a1', namespace: 'default' }, spec: {} })
      }),
    )
    const { result } = renderHook(() => useUpdateAgent(), { wrapper: createWrapper() })
    result.current.mutate({ name: 'a1', spec: { providerRef: { name: 'anthropic' } } })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(sawBody).toEqual({ spec: { providerRef: { name: 'anthropic' } } })
  })
})

describe('tool mutations', () => {
  it('creates, updates, and deletes tools', async () => {
    const wrapper = createWrapper()
    const create = renderHook(() => useCreateTool(), { wrapper })
    create.result.current.mutate({
      name: 'tavily',
      spec: { description: 'search', http: { url: 'https://api.tavily.com' } },
    })
    await waitFor(() => expect(create.result.current.isSuccess).toBe(true))

    const update = renderHook(() => useUpdateTool(), { wrapper })
    update.result.current.mutate({
      name: 'tavily',
      spec: { description: 'search v2', http: { url: 'https://api.tavily.com' } },
    })
    await waitFor(() => expect(update.result.current.isSuccess).toBe(true))

    const del = renderHook(() => useDeleteTool(), { wrapper })
    del.result.current.mutate('tavily')
    await waitFor(() => expect(del.result.current.isSuccess).toBe(true))
  })
})

describe('agent runtime registration', () => {
  it('registers, updates, and removes external runtimes', async () => {
    const wrapper = createWrapper()
    const create = renderHook(() => useCreateAgentRuntime(), { wrapper })
    create.result.current.mutate({
      metadata: { name: 'byo', namespace: 'default' },
      spec: { contractVersion: 'orka.harness.v2' },
    })
    await waitFor(() => expect(create.result.current.isSuccess).toBe(true))

    const update = renderHook(() => useUpdateAgentRuntime(), { wrapper })
    update.result.current.mutate({ name: 'byo', body: { spec: { contractVersion: 'orka.harness.v2' } } })
    await waitFor(() => expect(update.result.current.isSuccess).toBe(true))

    const del = renderHook(() => useDeleteAgentRuntime(), { wrapper })
    del.result.current.mutate('byo')
    await waitFor(() => expect(del.result.current.isSuccess).toBe(true))
  })
})

describe('substrate actor pools', () => {
  it('lists and reads pools', async () => {
    const wrapper = createWrapper()
    const list = renderHook(() => useSubstrateActorPools(false), { wrapper })
    await waitFor(() => expect(list.result.current.isSuccess).toBe(true))

    const detail = renderHook(() => useSubstrateActorPool('pool-a'), { wrapper })
    await waitFor(() => expect(detail.result.current.isSuccess).toBe(true))
    expect(detail.result.current.data?.status?.phase).toBe('Ready')
  })

  it('creates, updates, and deletes pools', async () => {
    const wrapper = createWrapper()
    const create = renderHook(() => useCreateSubstrateActorPool(), { wrapper })
    create.result.current.mutate({ name: 'pool-a', spec: { targetActors: 2 } })
    await waitFor(() => expect(create.result.current.isSuccess).toBe(true))

    const update = renderHook(() => useUpdateSubstrateActorPool(), { wrapper })
    update.result.current.mutate({ name: 'pool-a', spec: { targetActors: 4 } })
    await waitFor(() => expect(update.result.current.isSuccess).toBe(true))

    const del = renderHook(() => useDeleteSubstrateActorPool(), { wrapper })
    del.result.current.mutate('pool-a')
    await waitFor(() => expect(del.result.current.isSuccess).toBe(true))
  })
})

describe('monitor extensions', () => {
  it('updates a monitor spec', async () => {
    server.use(
      http.put('/api/v1/monitors/repositories/:name', () =>
        HttpResponse.json({ metadata: { name: 'mon', namespace: 'default' }, spec: {} }),
      ),
    )
    const { result } = renderHook(() => useUpdateRepositoryMonitor(), { wrapper: createWrapper() })
    result.current.mutate({ name: 'mon', spec: { provider: 'github' } })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })

  it('lists workflow events for a monitor', async () => {
    server.use(
      http.get('/api/v1/monitors/events', ({ request }) => {
        const url = new URL(request.url)
        expect(url.searchParams.get('name')).toBe('mon')
        return HttpResponse.json({
          items: [
            { id: 'ev1', eventType: 'run_started', summary: 'Run started', createdAt: '2026-06-13T00:00:00Z' },
          ],
          metadata: {},
        })
      }),
    )
    const { result } = renderHook(() => useRepositoryMonitorEvents('mon', false), {
      wrapper: createWrapper(),
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.items[0].eventType).toBe('run_started')
  })

  it('fetches an implementation job patch preview when enabled', async () => {
    server.use(
      http.get('/api/v1/monitors/implementation-jobs/:id/patch-preview', () =>
        HttpResponse.json({
          job: { id: 'job1' },
          patch: { files: [] },
          contentType: 'application/json',
        }),
      ),
    )
    const { result } = renderHook(() => useImplementationJobPatchPreview('job1', true), {
      wrapper: createWrapper(),
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.contentType).toBe('application/json')
  })
})

describe('security scan extensions', () => {
  it('updates and deletes a repository scan', async () => {
    server.use(
      http.put('/api/v1/security/repositories/:name', () =>
        HttpResponse.json({ metadata: { name: 'repo', namespace: 'default' }, spec: {} }),
      ),
      http.delete('/api/v1/security/repositories/:name', () => new HttpResponse(null, { status: 204 })),
    )
    const wrapper = createWrapper()
    const update = renderHook(() => useUpdateRepositoryScan(), { wrapper })
    update.result.current.mutate({ name: 'repo', spec: { repoURL: 'https://github.com/o/r' } })
    await waitFor(() => expect(update.result.current.isSuccess).toBe(true))

    const del = renderHook(() => useDeleteRepositoryScan(), { wrapper })
    del.result.current.mutate('repo')
    await waitFor(() => expect(del.result.current.isSuccess).toBe(true))
  })

  it('fetches a review slice detail', async () => {
    server.use(
      http.get('/api/v1/security/repositories/:name/slices/:sliceID', () =>
        HttpResponse.json({ id: 'slice-1', status: 'completed' }),
      ),
    )
    const { result } = renderHook(() => useReviewSliceDetail('repo', 'slice-1', true), {
      wrapper: createWrapper(),
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
