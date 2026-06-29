import { describe, it, expect, beforeEach, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useUIStore } from '@/stores/ui'
import { useTaskArtifacts, taskArtifactDownloadUrl } from './use-task-artifacts'

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

describe('useTaskArtifacts', () => {
  it('lists artifacts', async () => {
    server.use(http.get('/api/v1/tasks/t1/artifacts', () => HttpResponse.json({
      artifacts: [{ filename: 'a.txt', contentType: 'text/plain', size: 10, createdAt: '2026-06-28T00:00:00Z' }],
    })))
    const { result } = renderHook(() => useTaskArtifacts('t1'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.artifacts).toHaveLength(1)
  })

  it('polls when a refetch interval is provided', async () => {
    let calls = 0
    server.use(http.get('/api/v1/tasks/t-live/artifacts', () => {
      calls += 1
      return HttpResponse.json({
        artifacts: calls === 1
          ? []
          : [{ filename: 'later.txt', contentType: 'text/plain', size: 1 }],
      })
    }))

    const { result } = renderHook(() => useTaskArtifacts('t-live', true, undefined, 20), { wrapper: createWrapper() })

    await waitFor(() => expect(calls).toBeGreaterThan(1))
    await waitFor(() => expect(result.current.data?.artifacts[0]?.filename).toBe('later.txt'))
  })

  it('handles empty artifact response', async () => {
    server.use(http.get('/api/v1/tasks/t2/artifacts', () => HttpResponse.json({ artifacts: [] })))
    const { result } = renderHook(() => useTaskArtifacts('t2'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.artifacts).toEqual([])
  })
})

describe('taskArtifactDownloadUrl', () => {
  it('encodes filename and namespace', () => {
    expect(taskArtifactDownloadUrl('t1', 'a b.txt', 'ns')).toBe('/api/v1/tasks/t1/artifacts/a%20b.txt?namespace=ns')
  })
})
