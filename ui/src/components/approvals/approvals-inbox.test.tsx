import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, within } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import type { AttentionItem, AttentionResponse } from '@/schemas/attention'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, search, ...props }: any) => (
      <a href={String(to).replace('$taskId', params?.taskId ?? '')} data-search={search ? JSON.stringify(search) : undefined} {...props}>
        {children}
      </a>
    ),
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/approvals' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { ApprovalsInbox } from './approvals-inbox'

function approvalItem(name: string, canApprove: boolean | null, canDecline: boolean | null, extra: Partial<AttentionItem> = {}): AttentionItem {
  return {
    id: `approval:uid-${name}:ap-${name}`,
    kind: 'approval',
    rank: 1,
    since: new Date(Date.now() - 60_000).toISOString(),
    deadline: new Date(Date.now() + 10 * 60_000).toISOString(),
    summary: `Approval requested for tool-${name}`,
    task: { namespace: 'default', name, uid: `uid-${name}`, phase: 'Running' },
    approval: { id: `ap-${name}`, source: 'brokered', targetTool: `tool-${name}`, severity: 'warning', canApprove, canDecline },
    ...extra,
  }
}

function respond(items: AttentionItem[], extra: Partial<AttentionResponse> = {}) {
  server.use(
    http.get('/api/v1/approvals', ({ request }) => {
      const url = new URL(request.url)
      expect(url.searchParams.get('status')).toBe('pending')
      expect(url.searchParams.get('namespace')).toBe('default')
      return HttpResponse.json({
        namespace: 'default', generatedAt: new Date().toISOString(), items, total: items.length,
        counts: { approval: items.length }, incomplete: false,
        sources: { approvals: 'included', monitors: 'notRequested' }, metadata: {}, ...extra,
      })
    }),
  )
}

describe('ApprovalsInbox', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows decision controls only as the capability hints allow', async () => {
    respond([
      approvalItem('both', true, true),
      approvalItem('decline-only', false, true),
      approvalItem('unknown', null, null),
    ])
    render(<ApprovalsInbox />)
    const list = await screen.findByRole('list', { name: 'Pending approvals' })
    const rows = within(list).getAllByRole('listitem')
    expect(rows).toHaveLength(3)

    // Approving opens the Task's Approvals tab rather than deciding inline.
    const review = within(rows[0]).getByRole('link', { name: 'Review to approve' })
    expect(review).toHaveAttribute('href', '/tasks/both')
    expect(review).toHaveAttribute('data-search', JSON.stringify({ tab: 'approvals' }))
    expect(within(rows[0]).getByRole('button', { name: 'Decline' })).toBeInTheDocument()

    expect(within(rows[1]).queryByRole('link', { name: 'Review to approve' })).not.toBeInTheDocument()
    expect(within(rows[1]).getByRole('button', { name: 'Decline' })).toBeInTheDocument()

    expect(within(rows[2]).queryByRole('button')).not.toBeInTheDocument()
    expect(within(rows[2]).getByRole('link', { name: 'Open Task' })).toHaveAttribute('href', '/tasks/unknown')
  })

  it('declines through the per-Task decision route fenced to the Task UID', async () => {
    let posted: { url: URL; body: unknown } | undefined
    respond([approvalItem('task-a', true, true)])
    server.use(
      http.post('/api/v1/tasks/:id/approvals/:approvalID/decision', async ({ request }) => {
        posted = { url: new URL(request.url), body: await request.json() }
        return HttpResponse.json({ id: 'ap-task-a', status: 'declined', createdAt: new Date().toISOString(), action: 'x' })
      }),
    )
    render(<ApprovalsInbox />)
    await userEvent.click(await screen.findByRole('button', { name: 'Decline' }))
    await screen.findByText('Declined')
    expect(posted?.url.pathname).toBe('/api/v1/tasks/task-a/approvals/ap-task-a/decision')
    expect(posted?.url.searchParams.get('uid')).toBe('uid-task-a')
    expect(posted?.url.searchParams.get('namespace')).toBe('default')
    expect(posted?.body).toEqual({ decision: 'decline' })
  })

  it.each([409, 410])('treats %i as already decided', async (status) => {
    respond([approvalItem('task-b', false, true)])
    server.use(
      http.post('/api/v1/tasks/:id/approvals/:approvalID/decision', () =>
        HttpResponse.json({ error: { code: status, message: 'approval is already approved' } }, { status }),
      ),
    )
    render(<ApprovalsInbox />)
    await userEvent.click(await screen.findByRole('button', { name: 'Decline' }))
    expect(await screen.findByText('Already decided')).toBeInTheDocument()
  })

  it('shows parked sign-offs and never shows free text it was not given', async () => {
    respond([approvalItem('parked', true, true, {
      summary: 'Agent sign-off requested',
      approval: { id: 'ap-parked', source: 'parked', action: 'Agent sign-off (parked turn)', severity: 'info', canApprove: true, canDecline: true },
      task: { namespace: 'default', name: 'parked', uid: 'uid-parked', phase: 'Succeeded' },
    })])
    render(<ApprovalsInbox />)
    expect(await screen.findByText('Agent sign-off requested')).toBeInTheDocument()
    expect(screen.getByText('Parked sign-off')).toBeInTheDocument()
  })

  it('warns when the ranking is incomplete and handles empty, forbidden and unavailable states', async () => {
    respond([approvalItem('a', true, true)], { incomplete: true, incompleteReasons: ['approvalCandidateLimit'], total: 3 })
    const { unmount } = render(<ApprovalsInbox />)
    expect(await screen.findByText(/may be incomplete/)).toBeInTheDocument()
    expect(screen.getByText('Showing 1 of 3 pending approvals.')).toBeInTheDocument()
    unmount()

    respond([])
    const empty = render(<ApprovalsInbox />)
    expect(await screen.findByText('No pending approvals')).toBeInTheDocument()
    empty.unmount()

    server.use(http.get('/api/v1/approvals', () => HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })))
    const forbidden = render(<ApprovalsInbox />)
    expect(await screen.findByText('Not authorized to view approvals')).toBeInTheDocument()
    forbidden.unmount()

    server.use(http.get('/api/v1/approvals', () => HttpResponse.json({ error: { code: 501, message: 'off' } }, { status: 501 })))
    render(<ApprovalsInbox />)
    await waitFor(() => expect(screen.getByText('Approvals are unavailable')).toBeInTheDocument())
  })
})
