import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { render as rawRender } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { server } from '@/test/mocks/server'
import type { AttentionResponse } from '@/schemas/attention'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, search, ...props }: any) => (
      <a
        href={String(to).replace('$taskId', params?.taskId ?? '').replace('$monitorId', params?.monitorId ?? '')}
        data-search={search ? JSON.stringify(search) : undefined}
        {...props}
      >
        {children}
      </a>
    ),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { NeedsYouCard } from './needs-you-card'

function attention(extra: Partial<AttentionResponse> = {}): AttentionResponse {
  return {
    namespace: 'default', generatedAt: new Date().toISOString(), total: 1, counts: { failed: 1 }, incomplete: false,
    sources: { approvals: 'included', monitors: 'included' }, metadata: {},
    items: [{ id: 'f', kind: 'failed', rank: 3, since: new Date().toISOString(), summary: 'Task failed',
      task: { namespace: 'default', name: 'broken', uid: 'u-broken', phase: 'Failed' } }],
    ...extra,
  }
}

function renderWithClient() {
  // retryDelay 0 lets the hook's own retries for server errors settle quickly.
  const client = new QueryClient({ defaultOptions: { queries: { retryDelay: 0, gcTime: 0 } } })
  rawRender(<QueryClientProvider client={client}><NeedsYouCard /></QueryClientProvider>)
  return client
}

describe('NeedsYouCard', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows the top attention items with counts and links to the detail pages', async () => {
    server.use(
      http.get('/api/v1/attention', ({ request }) => {
        expect(new URL(request.url).searchParams.get('limit')).toBe('5')
        return HttpResponse.json({
          namespace: 'default', generatedAt: new Date().toISOString(), total: 7,
          counts: { approval: 2, failed: 4, monitorBlocked: 1 }, incomplete: true, incompleteReasons: ['taskScanLimit'],
          sources: { approvals: 'included', monitors: 'included' }, metadata: { continue: 'next' },
          items: [
            { id: 'a', kind: 'approval', rank: 1, since: new Date().toISOString(), summary: 'Agent sign-off requested',
              task: { namespace: 'default', name: 'parked', uid: 'u1', phase: 'Succeeded' },
              approval: { id: 'ap', source: 'parked', canApprove: true, canDecline: true } },
            { id: 'm', kind: 'monitorBlocked', rank: 5, since: new Date().toISOString(), summary: 'Monitor review blocked on pull_request #7',
              monitor: { name: 'repo', actionID: 'x' } },
          ],
        })
      }),
    )
    render(<NeedsYouCard />)
    expect(await screen.findByText('Agent sign-off requested')).toBeInTheDocument()
    expect(screen.getByText('7+')).toBeInTheDocument()
    expect(screen.getByText('2 approval · 4 failed · 1 monitor blocked')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Agent sign-off requested/ })).toHaveAttribute('href', '/tasks/parked')
    expect(screen.getByRole('link', { name: /Agent sign-off requested/ })).toHaveAttribute('data-search', JSON.stringify({ tab: 'approvals' }))
    expect(screen.getByRole('link', { name: /Monitor review blocked/ })).toHaveAttribute('href', '/monitors/repo')
    expect(screen.getByText(/may be incomplete/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Board' })).toHaveAttribute('data-search', JSON.stringify({ view: 'attention' }))
  })

  it('says when nothing needs attention and when the caller cannot list tasks', async () => {
    const { unmount } = render(<NeedsYouCard />)
    expect(await screen.findByText('Nothing needs you right now.')).toBeInTheDocument()
    unmount()
    server.use(http.get('/api/v1/attention', () => HttpResponse.json({ error: { code: 403, message: 'no' } }, { status: 403 })))
    render(<NeedsYouCard />)
    expect(await screen.findByText('Your token cannot list tasks in this namespace.')).toBeInTheDocument()
  })

  it('a 403 after a successful load replaces the cached items', async () => {
    server.use(http.get('/api/v1/attention', () => HttpResponse.json(attention())))
    const client = renderWithClient()
    expect(await screen.findByText('Task failed')).toBeInTheDocument()

    server.use(http.get('/api/v1/attention', () => HttpResponse.json({ error: { code: 403, message: 'no' } }, { status: 403 })))
    await client.refetchQueries({ queryKey: ['attention'] })

    expect(await screen.findByText('Your token cannot list tasks in this namespace.')).toBeInTheDocument()
    expect(screen.queryByText('Task failed')).not.toBeInTheDocument()
    expect(screen.queryByText('1')).not.toBeInTheDocument()
  })

  it('keeps the last loaded items with a stale note when a refresh fails transiently', async () => {
    server.use(http.get('/api/v1/attention', () => HttpResponse.json(attention())))
    const client = renderWithClient()
    expect(await screen.findByText('Task failed')).toBeInTheDocument()

    server.use(http.get('/api/v1/attention', () => HttpResponse.json({ error: { code: 500, message: 'etcd slow' } }, { status: 500 })))
    await client.refetchQueries({ queryKey: ['attention'] })

    expect(await screen.findByText(/Couldn't refresh attention items \(etcd slow\)/)).toBeInTheDocument()
    expect(screen.getByText('Task failed')).toBeInTheDocument()
  })

  it('qualifies the empty state when approvals were not read and notes hidden monitor actions quietly', async () => {
    server.use(http.get('/api/v1/attention', () =>
      HttpResponse.json(attention({ items: [], total: 0, counts: {}, sources: { approvals: 'unavailable', monitors: 'forbidden' } }))))
    renderWithClient()
    expect(await screen.findByText('Nothing else needs you right now, but approvals were not checked.')).toBeInTheDocument()
    expect(screen.queryByText('Nothing needs you right now.')).not.toBeInTheDocument()
    expect(screen.getByText(/Approvals could not be read/)).toHaveAttribute('role', 'status')
    const monitors = screen.getByText(/Blocked monitor actions are not shown: your token cannot list repository monitors/)
    expect(monitors).not.toHaveAttribute('role')
    expect(monitors).toHaveClass('text-muted-foreground')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('qualifies the empty state when only some approvals were checked', async () => {
    server.use(http.get('/api/v1/attention', () =>
      HttpResponse.json(attention({ items: [], total: 0, counts: {}, sources: { approvals: 'partial', monitors: 'included' } }))))
    renderWithClient()
    expect(await screen.findByText('Nothing else needs you right now, but some approvals could not be checked.')).toBeInTheDocument()
    expect(screen.queryByText('Nothing needs you right now.')).not.toBeInTheDocument()
    expect(screen.getByText(/^Some approvals could not be checked/)).toHaveAttribute('role', 'status')
    expect(screen.queryByText(/execution event storage/)).not.toBeInTheDocument()
  })
})
