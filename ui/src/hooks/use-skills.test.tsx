import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { http, HttpResponse } from 'msw'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import {
  useCreateSkill,
  useDeleteSkill,
  useSkill,
  useSkillContent,
  useSkillList,
  useUpdateSkill,
} from './use-skills'

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

describe('useSkillList', () => {
  it('lists skills', async () => {
    server.use(
      http.get('/api/v1/skills', () =>
        HttpResponse.json({
          items: [{ name: 'code-review', namespace: 'default', description: 'Review code', phase: 'Ready' }],
          metadata: {},
        }),
      ),
    )
    const { result } = renderHook(() => useSkillList(false), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.items[0].name).toBe('code-review')
  })
})

describe('useSkill / useSkillContent', () => {
  it('fetches skill detail', async () => {
    const { result } = renderHook(() => useSkill('code-review'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.spec.content.inline).toBe('# Skill')
  })

  it('fetches raw markdown content with auth header', async () => {
    useAuthStore.setState({ token: 'test-token' })
    server.use(
      http.get('/api/v1/skills/:name/content', ({ request }) => {
        expect(request.headers.get('Authorization')).toBe('Bearer test-token')
        return new HttpResponse('# Full content', {
          headers: { 'Content-Type': 'text/markdown' },
        })
      }),
    )
    const { result } = renderHook(() => useSkillContent('code-review'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toBe('# Full content')
  })

  it('surfaces content fetch failures', async () => {
    server.use(
      http.get('/api/v1/skills/:name/content', () => new HttpResponse(null, { status: 404 })),
    )
    const { result } = renderHook(() => useSkillContent('missing'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isError).toBe(true))
    expect(String(result.current.error)).toContain('404')
  })
})

describe('skill mutations', () => {
  it('creates, updates, and deletes skills', async () => {
    const wrapper = createWrapper()
    const create = renderHook(() => useCreateSkill(), { wrapper })
    create.result.current.mutate({
      name: 's1',
      spec: { description: 'd', content: { inline: '# S' } },
    })
    await waitFor(() => expect(create.result.current.isSuccess).toBe(true))

    const update = renderHook(() => useUpdateSkill(), { wrapper })
    update.result.current.mutate({
      name: 's1',
      spec: { description: 'd2', content: { inline: '# S2' } },
    })
    await waitFor(() => expect(update.result.current.isSuccess).toBe(true))

    const del = renderHook(() => useDeleteSkill(), { wrapper })
    del.result.current.mutate('s1')
    await waitFor(() => expect(del.result.current.isSuccess).toBe(true))
  })
})
