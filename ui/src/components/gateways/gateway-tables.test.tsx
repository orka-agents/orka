import type { ReactNode } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { render, screen, waitFor } from '@/test/test-utils'
import { server } from '@/test/mocks/server'
import type { GatewayBinding, GatewayDelivery, GatewayEvent } from '@/schemas/gateway'
import { GatewayBindingsTable, GatewayDeliveriesTable, GatewayEventsTable } from './gateway-tables'

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, className }: { children: ReactNode; to: string; params?: Record<string, string>; className?: string }) => {
      let href = to
      for (const [name, value] of Object.entries(params ?? {})) href = href.replace(`$${name}`, value)
      return <a href={href} className={className}>{children}</a>
    },
  }
})

const binding: GatewayBinding = {
  metadata: { name: 'support-binding', namespace: 'default', generation: 3 },
  spec: {
    gatewayRef: { name: 'chat' },
    agentRef: { name: 'operator' },
    match: { accountId: 'acme', contextId: 'support' },
  },
  status: { ready: true, observedGeneration: 3 },
}

const event: GatewayEvent = {
  id: 'event-1',
  namespace: 'default',
  gatewayName: 'chat',
  externalEventId: 'provider-event-1',
  state: 'TaskCreated',
  accountId: 'acme',
  contextId: 'support',
  senderId: 'sender-1',
  sessionName: 'gateway-session-1',
  taskName: 'task-1',
  attemptCount: 1,
  receivedAt: '2026-07-18T10:00:00Z',
  expiresAt: '2026-08-01T00:00:00Z',
  createdAt: '2026-07-18T10:00:00Z',
  updatedAt: '2026-07-18T10:00:00Z',
}

describe('gateway tables', () => {
  it('links binding rows to the GatewayBinding operator detail', () => {
    render(<GatewayBindingsTable bindings={[binding]} loading={false} />)

    expect(screen.getByRole('link', { name: /support-binding/i })).toHaveAttribute('href', '/gateways/bindings/support-binding')
  })

  it('does not present a stale GatewayBinding status as Ready', () => {
    render(
      <GatewayBindingsTable
        bindings={[{
          ...binding,
          metadata: { ...binding.metadata, generation: 4 },
          status: { ready: true, observedGeneration: 3 },
        }]}
        loading={false}
      />,
    )

    expect(screen.getByText('NotReady')).toBeInTheDocument()
    expect(screen.queryByText('Ready')).not.toBeInTheDocument()
  })

  it('keeps protected gateway Sessions off the generic Session route', () => {
    render(<GatewayEventsTable events={[event]} loading={false} />)

    expect(screen.getByText('gateway-session-1').closest('a')).toBeNull()
    expect(screen.getByText('gateway-managed')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'task-1' })).toHaveAttribute('href', '/tasks/task-1')
    expect(screen.queryByRole('link', { name: 'gateway-session-1' })).not.toBeInTheDocument()
  })

  it('shows the newer outbound activity when it follows the last inbound event', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-07-18T10:00:00Z'))
    try {
      render(
        <GatewayBindingsTable
          bindings={[{
            ...binding,
            status: {
              ...binding.status,
              lastInboundActivity: '2026-07-18T08:00:00Z',
              lastOutboundActivity: '2026-07-18T09:59:00Z',
            },
          }]}
          loading={false}
        />,
      )

      expect(screen.getByText('1m ago')).toBeInTheDocument()
      expect(screen.queryByText('2h ago')).not.toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('surfaces manual delivery retry failures', async () => {
    const delivery: GatewayDelivery = {
      id: 'delivery-failed', namespace: 'default', gatewayName: 'chat', eventId: 'event-1',
      kind: 'error', state: 'DeadLettered', replyTarget: 'room', text: 'failed', attemptCount: 10,
      maxAttempts: 10, manualRetryCount: 0, nextAttemptAt: '2026-07-18T10:00:00Z',
      expiresAt: '2026-07-19T10:00:00Z', createdAt: '2026-07-18T09:00:00Z', updatedAt: '2026-07-18T10:00:00Z',
    }
    server.use(http.post('/api/v1/gateway-deliveries/delivery-failed/retry', () => (
      HttpResponse.text('retry denied', { status: 409 })
    )))

    render(<GatewayDeliveriesTable deliveries={[delivery]} loading={false} />)
    await userEvent.click(screen.getByRole('button', { name: 'Retry delivery-failed' }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(
      expect.stringContaining('retry denied'),
    ))
  })
})
