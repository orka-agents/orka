import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse, delay } from 'msw'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { TaskApprovalPanel } from './task-approval-panel'

const API = '/api/v1'

function approval(overrides: Record<string, unknown> = {}) {
  return {
    id: 'ap-1',
    action: 'web_fetch',
    riskSummary: 'fetch an external URL',
    status: 'pending',
    createdAt: '2026-06-13T00:00:00Z',
    ...overrides,
  }
}

function listResponse(approvals: Record<string, unknown>[]) {
  return { namespace: 'default', taskName: 'tk', approvals }
}

describe('TaskApprovalPanel', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
  })

  it('renders a pending approval with approve and decline buttons', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))),
    )
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByTestId('approval-card')).toBeInTheDocument())
    expect(screen.getByText('fetch an external URL')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Decline' })).toBeInTheDocument()
  })

  it('approve sends the correct request and updates the UI', async () => {
    let capturedBody: unknown = null
    let capturedPath = ''
    let listCalls = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => {
        listCalls += 1
        // First load pending; after the decision, return approved.
        return HttpResponse.json(listResponse([approval({ status: listCalls > 1 ? 'approved' : 'pending' })]))
      }),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, async ({ request }) => {
        capturedBody = await request.json()
        capturedPath = new URL(request.url).pathname
        return HttpResponse.json(approval({ status: 'approved', decisionActor: 'alice' }))
      }),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(capturedBody).toEqual({ decision: 'approve' }))
    expect(capturedPath).toBe('/api/v1/tasks/tk/approvals/ap-1/decision')
    // After invalidation the card reloads as approved (no more action buttons).
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument())
  })

  it('decline sends the decline decision with an optional reason', async () => {
    let capturedBody: unknown = null
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, async ({ request }) => {
        capturedBody = await request.json()
        return HttpResponse.json(approval({ status: 'declined' }))
      }),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Decline' })).toBeInTheDocument())
    await user.type(screen.getByLabelText('Decision reason'), 'too risky')
    await user.click(screen.getByRole('button', { name: 'Decline' }))
    await waitFor(() => expect(capturedBody).toEqual({ decision: 'decline', reason: 'too risky' }))
  })

  it('hides decision buttons for an already-terminal approval', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () =>
        HttpResponse.json(listResponse([approval({ status: 'approved', decisionActor: 'bob', decisionReason: 'looks fine' })])),
      ),
    )
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByText('Approved')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Decline' })).not.toBeInTheDocument()
    expect(screen.getByText('bob')).toBeInTheDocument()
    expect(screen.getByText('looks fine')).toBeInTheDocument()
  })

  it('surfaces a conflict when the approval was already decided elsewhere (409)', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, () =>
        new HttpResponse('approval is already declined', { status: 409 }),
      ),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() =>
      expect(screen.getByText(/approval is already declined/i)).toBeInTheDocument(),
    )
  })

  it('disables the action buttons while a decision is in flight', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, async () => {
        await delay(100)
        return HttpResponse.json(approval({ status: 'approved' }))
      }),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument())
    const approveBtn = screen.getByRole('button', { name: 'Approve' })
    await user.click(approveBtn)
    // While the request is in flight, both buttons are disabled.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Decline' })).toBeDisabled())
    expect(screen.getByRole('button', { name: 'Approve' })).toBeDisabled()
  })

  it('shows an empty state when there are no approvals', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([]))),
    )
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByText('No approvals requested.')).toBeInTheDocument())
  })

  it('shows a pending count badge when approvals await decision', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () =>
        HttpResponse.json(listResponse([approval({ id: 'ap-1' }), approval({ id: 'ap-2' })])),
      ),
    )
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByText('2 pending')).toBeInTheDocument())
  })

  it('shows an error state with retry on failure', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => new HttpResponse('boom', { status: 500 })),
    )
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByText('Failed to load approvals.')).toBeInTheDocument())
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })
})
