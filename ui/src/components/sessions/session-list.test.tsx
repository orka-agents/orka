import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/sessions' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { render, screen, waitFor } from '@/test/test-utils'
import { toast } from 'sonner'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { SessionList } from './session-list'

describe('SessionList', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
    vi.restoreAllMocks()
  })

  it('shows skeleton rows while loading', () => {
    // Default handler returns empty list; check skeletons appear before data loads
    const { container } = render(<SessionList />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('shows "No sessions found." for empty list', async () => {
    render(<SessionList />)
    await waitFor(() => {
      expect(screen.getByText('No sessions found.')).toBeInTheDocument()
    })
  })

  it('shows a not-authorized message (not "No sessions found.") on 403', async () => {
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      ),
    )
    render(<SessionList />)
    await waitFor(() => {
      expect(screen.getByText('Not authorized to view sessions')).toBeInTheDocument()
    })
    expect(screen.getByText(/read permission for sessions \(not authorized\)/)).toBeInTheDocument()
    expect(screen.queryByText('No sessions found.')).not.toBeInTheDocument()
    expect(screen.queryByText(/"code":403/)).not.toBeInTheDocument()
  })

  it('shows a load error for non-403 failures', async () => {
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({ error: { code: 400, message: 'invalid namespace' } }, { status: 400 }),
      ),
    )
    render(<SessionList />)
    await waitFor(() => {
      expect(screen.getByText('Could not load sessions')).toBeInTheDocument()
      expect(screen.getByText('invalid namespace')).toBeInTheDocument()
    })
  })

  it('renders populated table with session data', async () => {
    server.use(
      http.get('/api/v1/sessions', () => {
        return HttpResponse.json({
          items: [
            {
              name: 'sess-abc',
              namespace: 'default',
              messageCount: '12',
              inputTokens: '500',
              outputTokens: '800',
              activeTask: 'my-task',
              createdAt: new Date().toISOString(),
            },
          ],
          metadata: {},
        })
      })
    )

    render(<SessionList />)
    await waitFor(() => {
      expect(screen.getByText('sess-abc')).toBeInTheDocument()
    })
    expect(screen.getByText('12')).toBeInTheDocument()
    expect(screen.getByText('500 / 800')).toBeInTheDocument()
    expect(screen.getByText('my-task')).toBeInTheDocument()
  })

  it('has a delete button for each session row', async () => {
    server.use(
      http.get('/api/v1/sessions', () => {
        return HttpResponse.json({
          items: [
            { name: 'sess-1', namespace: 'default' },
            { name: 'sess-2', namespace: 'default' },
          ],
          metadata: {},
        })
      })
    )

    render(<SessionList />)
    await waitFor(() => {
      expect(screen.getByText('sess-1')).toBeInTheDocument()
    })
    const buttons = screen.getAllByRole('button')
    expect(buttons.length).toBe(2)
  })

  it('renders table headers', () => {
    render(<SessionList />)
    expect(screen.getByText('Name')).toBeInTheDocument()
    expect(screen.getByText('Namespace')).toBeInTheDocument()
    expect(screen.getByText('Messages')).toBeInTheDocument()
    // The delete column's header is named for screen readers, not left empty.
    expect(screen.getByRole('columnheader', { name: 'Actions' })).toBeInTheDocument()
  })

  it('names each delete button by its session', async () => {
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({ items: [{ name: 'sess-1', namespace: 'default' }], metadata: {} }),
      ),
    )
    render(<SessionList />)
    expect(await screen.findByRole('button', { name: 'Delete session sess-1' })).toBeInTheDocument()
  })

  it('asks for confirmation and does nothing when declined', async () => {
    const user = userEvent.setup()
    let deletes = 0
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({ items: [{ name: 'sess-keep', namespace: 'default' }], metadata: {} }),
      ),
      http.delete('/api/v1/sessions/:id', () => {
        deletes += 1
        return new HttpResponse(null, { status: 204 })
      }),
    )
    render(<SessionList />)
    await user.click(await screen.findByRole('button', { name: 'Delete session sess-keep' }))
    expect(window.confirm).toHaveBeenCalledWith('Delete session "sess-keep"?')
    expect(deletes).toBe(0)
  })

  it('deletes a confirmed session and reports success', async () => {
    const user = userEvent.setup()
    let deleted: string | undefined
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({ items: [{ name: 'sess-del', namespace: 'default', createdAt: new Date().toISOString() }], metadata: {} }),
      ),
      http.delete('/api/v1/sessions/:id', ({ params }) => {
        deleted = params.id as string
        return new HttpResponse(null, { status: 204 })
      }),
    )
    render(<SessionList />)
    await user.click(await screen.findByRole('button', { name: 'Delete session sess-del' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Session deleted'))
    expect(deleted).toBe('sess-del')
  })

  it('blocks a repeat delete of a session while another row is deleting', async () => {
    const user = userEvent.setup()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const deletes: string[] = []
    const releases: Array<() => void> = []
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({
          items: [
            { name: 'sess-a', namespace: 'default', createdAt: new Date().toISOString() },
            { name: 'sess-b', namespace: 'default', createdAt: new Date().toISOString() },
          ],
          metadata: {},
        }),
      ),
      http.delete('/api/v1/sessions/:id', ({ params }) => {
        deletes.push(params.id as string)
        return new Promise<HttpResponse>((resolve) => releases.push(() => resolve(new HttpResponse(null, { status: 204 }))))
      }),
    )
    render(<SessionList />)
    await user.click(await screen.findByRole('button', { name: 'Delete session sess-a' }))
    await user.click(screen.getByRole('button', { name: 'Delete session sess-b' }))
    // sess-b's delete must not re-enable sess-a's pending one.
    await user.click(screen.getByRole('button', { name: 'Delete session sess-a' }))
    await waitFor(() => expect(deletes).toEqual(['sess-a', 'sess-b']))
    releases.forEach((release) => release())
    await waitFor(() => expect(toast.success).toHaveBeenCalledTimes(2))
    expect(deletes).toEqual(['sess-a', 'sess-b'])
  })

  it('shows the server message when a delete is refused', async () => {
    const user = userEvent.setup()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({ items: [{ name: 'sess-ro', namespace: 'default' }], metadata: {} }),
      ),
      http.delete('/api/v1/sessions/:id', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      ),
    )
    render(<SessionList />)
    await user.click(await screen.findByRole('button', { name: 'Delete session sess-ro' }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete session: not authorized'))
  })

  it('keeps keyboard focus on the trash button while a refused delete is in flight', async () => {
    const user = userEvent.setup()
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    let deletes = 0
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({ items: [{ name: 'sess-ro', namespace: 'default' }], metadata: {} }),
      ),
      http.delete('/api/v1/sessions/:id', async () => {
        deletes += 1
        await gate
        return HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })
      }),
    )
    render(<SessionList />)
    const trash = await screen.findByRole('button', { name: 'Delete session sess-ro' })
    trash.focus()
    await user.keyboard('{Enter}')

    // Pending: marked unavailable but never disabled, so focus stays put.
    await waitFor(() => expect(trash).toHaveAttribute('aria-disabled', 'true'))
    expect(trash).not.toBeDisabled()
    expect(trash).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(deletes).toBe(1)

    release()
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete session: not authorized'))
    await waitFor(() => expect(trash).not.toHaveAttribute('aria-disabled'))
    expect(trash).toHaveFocus()
  })

  it('renders sessions in the server order (newest first), not sorted by name, across pages', async () => {
    const user = userEvent.setup()
    const now = Date.now()
    server.use(
      http.get('/api/v1/sessions', ({ request }) => {
        const page2 = new URL(request.url).searchParams.get('continue') === 'next'
        return HttpResponse.json(page2
          ? { items: [{ name: 'sess-bravo', namespace: 'default', createdAt: new Date(now - 300_000).toISOString() }], metadata: {} }
          : {
              items: [
                { name: 'sess-zulu', namespace: 'default', createdAt: new Date(now - 60_000).toISOString() },
                { name: 'sess-alpha', namespace: 'default', createdAt: new Date(now - 120_000).toISOString() },
                { name: 'sess-mike', namespace: 'default', createdAt: new Date(now - 180_000).toISOString() },
              ],
              metadata: { continue: 'next' },
            })
      }),
    )
    render(<SessionList />)
    const rowNames = () => screen.getAllByRole('link').map((link) => link.textContent)
    await waitFor(() => expect(rowNames()).toEqual(['sess-zulu', 'sess-alpha', 'sess-mike']))
    await user.click(screen.getByRole('button', { name: 'Load more' }))
    await waitFor(() => expect(rowNames()).toEqual(['sess-zulu', 'sess-alpha', 'sess-mike', 'sess-bravo']))
  })

  it('timeAgo covers minutes, hours, and days', async () => {
    const now = Date.now()
    server.use(
      http.get('/api/v1/sessions', () =>
        HttpResponse.json({
          items: [
            { name: 's-min', namespace: 'default', createdAt: new Date(now - 120_000).toISOString() },
            { name: 's-hr', namespace: 'default', createdAt: new Date(now - 7200_000).toISOString() },
            { name: 's-day', namespace: 'default', createdAt: new Date(now - 172800_000).toISOString() },
          ],
          metadata: {},
        }),
      ),
    )
    render(<SessionList />)
    await waitFor(() => {
      expect(screen.getByText('s-min')).toBeInTheDocument()
    })
    expect(screen.getByText('2m')).toBeInTheDocument()
    expect(screen.getByText('2h')).toBeInTheDocument()
    expect(screen.getByText('2d')).toBeInTheDocument()
  })
})
