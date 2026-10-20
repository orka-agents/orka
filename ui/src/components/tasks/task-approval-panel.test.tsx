import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse, delay } from 'msw'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { toast } from 'sonner'
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

  it('breaks a long approval ID instead of overflowing the card', async () => {
    const id = 'a'.repeat(64)
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval({ id })]))),
    )
    render(<TaskApprovalPanel taskId="tk" />)
    expect(await screen.findByText(id)).toHaveClass('break-all')
  })

  it('shows the wrapped operation, safe inputs, and an unknown execution outcome', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval({
        action: 'Execute create_work_order via call-tool',
        targetTool: 'call-tool',
        targetArgsPreview: { name: 'create_work_order', inputs: { machine: 'simulated-1', token: '[REDACTED]' } },
        status: 'approved',
        executionOutcome: 'unknown',
        executionReason: 'The action may have run. Do not repeat it automatically.',
      })]))),
    )
    render(<TaskApprovalPanel taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(screen.getByText('Execute create_work_order via call-tool')).toBeInTheDocument())
    expect(screen.getByText('Proposed inputs for call-tool')).toBeInTheDocument()
    expect(screen.getByText(/simulated-1/)).toHaveTextContent('[REDACTED]')
    expect(screen.getByText('Execution: unknown')).toBeInTheDocument()
    expect(screen.getByText('The action may have run. Check its outcome before trying again.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
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
    expect(screen.getByText('fetch an external URL')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(capturedBody).toEqual({ decision: 'approve' }))
    expect(capturedPath).toBe('/api/v1/tasks/tk/approvals/ap-1/decision')
    // After invalidation the card reloads as approved (no more action buttons).
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument())
    expect(screen.queryByText('fetch an external URL')).not.toBeInTheDocument()
  })

  it.each([
    { status: 'pending', executionOutcome: 'not_started' },
    { status: 'approved', executionOutcome: 'executing' },
    { status: 'approved', executionOutcome: 'succeeded' },
    { status: 'approved', executionOutcome: 'failed' },
    { status: 'approved', executionOutcome: 'unknown' },
    { status: 'declined', executionOutcome: 'not_started' },
    { status: 'cancelled', executionOutcome: 'not_started' },
    { status: 'cancelled', executionOutcome: 'unknown' },
    { status: 'expired', executionOutcome: 'not_started' },
  ])('shows current execution state for $status/$executionOutcome', async ({ status, executionOutcome }) => {
    const riskSummary = 'Review this exact operation and its inputs. The tool has not run.'
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval({
        riskSummary, status, executionOutcome,
        targetTool: 'create-work-order', targetArgsPreview: { asset: 'pump-1' },
      })]))),
    )
    render(<TaskApprovalPanel taskId="tk" taskPhase="Running" />)
    await waitFor(() => expect(screen.getByText(`Execution: ${executionOutcome.replace(/_/g, ' ')}`)).toBeInTheDocument())
    expect(screen.getByText('Proposed inputs for create-work-order')).toBeInTheDocument()
    expect(screen.getByText(/pump-1/)).toBeInTheDocument()
    if (status === 'pending') {
      expect(screen.getByText(riskSummary)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument()
    } else {
      expect(screen.queryByText(riskSummary)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    }
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

  it('refetches approvals after a 409 so the card reflects the settled state', async () => {
    let decided = false
    let getCalls = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => {
        getCalls += 1
        // After the 409, the refetch returns the settled (declined) approval.
        return HttpResponse.json(listResponse([approval({ status: decided ? 'declined' : 'pending' })]))
      }),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, () => {
        decided = true
        return new HttpResponse('approval is already declined', { status: 409 })
      }),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument())
    const callsBefore = getCalls
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    // The 409 triggers a refetch (not waiting for the 5s poll), and the card
    // updates to the settled state — no more action buttons.
    await waitFor(() => expect(getCalls).toBeGreaterThan(callsBefore))
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument())
    // Once the card has settled, the stale conflict note goes away.
    expect(screen.queryByText(/approval is already declined/i)).not.toBeInTheDocument()
  })

  it('does not double the card top padding', async () => {
    server.use(http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))))
    render(<TaskApprovalPanel taskId="tk" />)
    const card = await screen.findByTestId('approval-card')
    // Card supplies py-6; a pt-6 on its first CardContent doubled it.
    expect(card.querySelector('[data-slot="card-content"]')).not.toHaveClass('pt-6')
  })

  it('confirms a decision in plain words', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, () => HttpResponse.json(approval({ status: 'approved' }))),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Decision recorded: Approved'))
  })

  it('blocks repeat decisions while one is in flight without dropping focus', async () => {
    let posts = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, async () => {
        posts += 1
        await delay(100)
        return HttpResponse.json(approval({ status: 'approved' }))
      }),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument())
    const approveBtn = screen.getByRole('button', { name: 'Approve' })
    await user.click(approveBtn)
    // While the request is in flight, both buttons read as disabled but the
    // pressed one keeps focus (a disabled element would drop it).
    await waitFor(() => expect(screen.getByRole('button', { name: 'Decline' })).toHaveAttribute('aria-disabled', 'true'))
    expect(approveBtn).toHaveAttribute('aria-disabled', 'true')
    expect(approveBtn).toHaveFocus()
    await user.click(approveBtn)
    await user.click(screen.getByRole('button', { name: 'Decline' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Decision recorded: Approved'))
    expect(posts).toBe(1)
  })

  it('moves keyboard focus to the decided card instead of dropping it', async () => {
    let listCalls = 0
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => {
        listCalls += 1
        return HttpResponse.json(listResponse([approval({ status: listCalls > 1 ? 'approved' : 'pending' })]))
      }),
      http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, () =>
        HttpResponse.json(approval({ status: 'approved' }))),
    )
    const user = userEvent.setup()
    render(<TaskApprovalPanel taskId="tk" />)
    const approveBtn = await screen.findByRole('button', { name: 'Approve' })
    approveBtn.focus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument())
    expect(screen.getByRole('heading', { level: 3, name: 'web_fetch' })).toHaveFocus()
    expect(document.body).not.toHaveFocus()
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

  it('renders a pending approval read-only when the task has completed', async () => {
    server.use(
      http.get(`${API}/tasks/:id/approvals`, () => HttpResponse.json(listResponse([approval()]))),
    )
    // The task is terminal, so the backend would 409 any decision — the card must
    // not offer Approve/Decline.
    render(<TaskApprovalPanel taskId="tk" taskPhase="Succeeded" />)
    await waitFor(() => expect(screen.getByTestId('approval-card')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Decline' })).not.toBeInTheDocument()
    expect(screen.getByText(/can no longer be decided because the task has completed/i)).toBeInTheDocument()
  })
})
