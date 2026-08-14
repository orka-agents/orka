import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { ProposalInbox } from './proposal-inbox'

function proposal(overrides: Record<string, unknown> = {}) {
  return {
    id: 'p1',
    namespace: 'default',
    agentName: 'coder',
    type: 'memory',
    title: 'Remember the deploy flow',
    description: 'Captured after task success',
    content: 'Deploys require make manifests first',
    status: 'pending',
    createdAt: '2026-06-13T00:00:00Z',
    updatedAt: '2026-06-13T00:00:00Z',
    ...overrides,
  }
}

describe('ProposalInbox', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('explains the review-then-apply governance rule', async () => {
    render(<ProposalInbox />)
    expect(
      await screen.findByText(/applying an accepted proposal is the separate, explicit step/i),
    ).toBeInTheDocument()
  })

  it('accepts a pending proposal with a note', async () => {
    let reviewBody: any
    server.use(
      http.get('/api/v1/memory-proposals', () =>
        HttpResponse.json({ items: [proposal()], metadata: {} }),
      ),
      http.post('/api/v1/memory-proposals/:id/review', async ({ request }) => {
        reviewBody = await request.json()
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<ProposalInbox />)
    await waitFor(() => expect(screen.getByText('Remember the deploy flow')).toBeInTheDocument())
    await user.type(screen.getByLabelText('Review note'), 'verified against repo docs')
    await user.click(screen.getByRole('button', { name: 'Accept' }))
    await waitFor(() => expect(reviewBody).toBeTruthy())
    expect(reviewBody).toEqual({ status: 'accepted', reviewNote: 'verified against repo docs' })
  })

  it('shows Apply only for accepted proposals and applies them', async () => {
    let applied = false
    server.use(
      http.get('/api/v1/memory-proposals', () =>
        HttpResponse.json({ items: [proposal({ status: 'accepted', reviewer: 'admin' })], metadata: {} }),
      ),
      http.post('/api/v1/memory-proposals/:id/apply', () => {
        applied = true
        return HttpResponse.json({
          id: 'mem-9',
          namespace: 'default',
          content: 'Deploys require make manifests first',
          createdAt: '2026-06-13T00:00:00Z',
          updatedAt: '2026-06-13T00:00:00Z',
        })
      }),
    )
    const user = userEvent.setup()
    render(<ProposalInbox />)
    // Default filter is pending — switch to accepted.
    await user.click(screen.getByRole('button', { name: 'accepted' }))
    await waitFor(() => expect(screen.getByRole('button', { name: /apply to memory/i })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /apply to memory/i }))
    await waitFor(() => expect(applied).toBe(true))
  })

  it('expands proposed content on demand', async () => {
    server.use(
      http.get('/api/v1/memory-proposals', () =>
        HttpResponse.json({ items: [proposal()], metadata: {} }),
      ),
    )
    const user = userEvent.setup()
    render(<ProposalInbox />)
    await waitFor(() => expect(screen.getByText('Remember the deploy flow')).toBeInTheDocument())
    expect(screen.queryByText(/deploys require make manifests/i)).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /show proposed content/i }))
    expect(screen.getByText(/deploys require make manifests/i)).toBeInTheDocument()
  })

  it('renders the not-enabled state on 501', async () => {
    server.use(
      http.get('/api/v1/memory-proposals', () =>
        HttpResponse.json({ error: { code: 501, message: 'memory proposal store not configured' } }, { status: 501 }),
      ),
    )
    render(<ProposalInbox />)
    await waitFor(() => expect(screen.getByText(/proposal store is not enabled/i)).toBeInTheDocument())
  })
})

describe('ProposalInbox decisions', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('rejects a pending proposal', async () => {
    let reviewBody: any
    server.use(
      http.get('/api/v1/memory-proposals', () =>
        HttpResponse.json({ items: [proposal()], metadata: {} }),
      ),
      http.post('/api/v1/memory-proposals/:id/review', async ({ request }) => {
        reviewBody = await request.json()
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<ProposalInbox />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Reject' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Reject' }))
    await waitFor(() => expect(reviewBody).toBeTruthy())
    expect(reviewBody.status).toBe('rejected')
  })

  it('archives a rejected proposal', async () => {
    let archived = false
    server.use(
      http.get('/api/v1/memory-proposals', () =>
        HttpResponse.json({ items: [proposal({ status: 'rejected' })], metadata: {} }),
      ),
      http.post('/api/v1/memory-proposals/:id/archive', () => {
        archived = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<ProposalInbox />)
    await user.click(screen.getByRole('button', { name: 'rejected' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Archive' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Archive' }))
    await waitFor(() => expect(archived).toBe(true))
  })

  it('shows applied linkage on applied proposals', async () => {
    server.use(
      http.get('/api/v1/memory-proposals', () =>
        HttpResponse.json({
          items: [proposal({ status: 'applied', appliedMemoryId: 'mem-7', reviewer: 'admin' })],
          metadata: {},
        }),
      ),
    )
    const user = userEvent.setup()
    render(<ProposalInbox />)
    await user.click(screen.getByRole('button', { name: 'applied' }))
    await waitFor(() => expect(screen.getByText(/memory mem-7/)).toBeInTheDocument())
  })
})
