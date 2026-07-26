import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@/test/test-utils'
import type { GatewayEvent } from '@/schemas/gateway'
import { GatewaySessionQueue } from './gateway-session-queue'

function event(overrides: Partial<GatewayEvent> & Pick<GatewayEvent, 'id' | 'externalEventId' | 'state' | 'receivedAt'>): GatewayEvent {
  return {
    namespace: 'default',
    gatewayName: 'chat',
    accountId: 'acme',
    contextId: 'support',
    senderId: 'sender-1',
    attemptCount: 0,
    expiresAt: '2026-08-01T00:00:00Z',
    createdAt: overrides.receivedAt,
    updatedAt: overrides.receivedAt,
    ...overrides,
  }
}

describe('GatewaySessionQueue', () => {
  it('groups active records by Session and orders each visible queue oldest first', () => {
    render(
      <GatewaySessionQueue
        loading={false}
        events={[
          event({ id: 'second', externalEventId: 'external-second', state: 'Queued', sessionName: 'session-alpha', transcriptOrder: 12, receivedAt: '2026-07-18T10:01:00Z' }),
          event({ id: 'terminal', externalEventId: 'external-complete', state: 'Completed', sessionName: 'session-alpha', transcriptOrder: 8, receivedAt: '2026-07-18T09:00:00Z' }),
          event({ id: 'beta', externalEventId: 'external-beta', state: 'Accepted', sessionName: 'session-beta', receivedAt: '2026-07-18T10:00:00Z' }),
          event({ id: 'first', externalEventId: 'external-first', state: 'TaskCreated', sessionName: 'session-alpha', transcriptOrder: 10, receivedAt: '2026-07-18T10:02:00Z' }),
          event({ id: 'unassigned', externalEventId: 'external-unassigned', state: 'Dispatching', receivedAt: '2026-07-18T10:03:00Z' }),
        ]}
      />,
    )

    expect(screen.getByText('Current page only')).toBeInTheDocument()
    expect(screen.getByText(/not namespace totals/i)).toBeInTheDocument()
    expect(screen.queryByText('external-complete')).not.toBeInTheDocument()

    const alpha = screen.getByRole('region', { name: 'session-alpha visible FIFO queue' })
    const alphaRecords = within(alpha).getAllByRole('listitem')
    expect(within(alphaRecords[0]).getByText('external-first')).toBeInTheDocument()
    expect(within(alphaRecords[0]).getByText('Session order 10')).toBeInTheDocument()
    expect(within(alphaRecords[1]).getByText('external-second')).toBeInTheDocument()
    expect(within(alpha).getByText(/oldest visible event first/i)).toBeInTheDocument()

    expect(screen.getByRole('region', { name: 'session-beta visible FIFO queue' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Awaiting Session assignment visible FIFO queue' })).toBeInTheDocument()
  })

  it('uses a transitive total order when transcript positions are mixed with unpositioned records', () => {
    render(
      <GatewaySessionQueue
        loading={false}
        events={[
          event({ id: 'ordered-two', externalEventId: 'ordered-two', state: 'Queued', sessionName: 'mixed', transcriptOrder: 2, receivedAt: '2026-07-18T10:00:00Z' }),
          event({ id: 'unpositioned', externalEventId: 'unpositioned', state: 'Queued', sessionName: 'mixed', receivedAt: '2026-07-18T09:00:00Z' }),
          event({ id: 'ordered-zero', externalEventId: 'ordered-zero', state: 'Queued', sessionName: 'mixed', transcriptOrder: 0, receivedAt: '2026-07-18T12:00:00Z' }),
          event({ id: 'ordered-one', externalEventId: 'ordered-one', state: 'Queued', sessionName: 'mixed', transcriptOrder: 1, receivedAt: '2026-07-18T11:00:00Z' }),
        ]}
      />,
    )

    const queue = screen.getByRole('region', { name: 'mixed visible FIFO queue' })
    const records = within(queue).getAllByRole('listitem')
    expect(within(records[0]).getByText('ordered-zero')).toBeInTheDocument()
    expect(within(records[0]).getByText('Session order 0')).toBeInTheDocument()
    expect(within(records[1]).getByText('ordered-one')).toBeInTheDocument()
    expect(within(records[2]).getByText('ordered-two')).toBeInTheDocument()
    expect(within(records[3]).getByText('unpositioned')).toBeInTheDocument()
  })
})
