import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

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
})
