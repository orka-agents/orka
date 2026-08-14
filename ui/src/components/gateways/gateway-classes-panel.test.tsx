import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { GatewayClassesPanel } from './gateway-classes-panel'

describe('GatewayClassesPanel', () => {
  it('shows an empty state', async () => {
    render(<GatewayClassesPanel />)
    await waitFor(() => expect(screen.getByText(/no gateway classes/i)).toBeInTheDocument())
  })

  it('lists classes with capabilities', async () => {
    server.use(
      http.get('/api/v1/gatewayclasses', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'slack' },
              spec: {
                contractVersion: 'orka.gateway.v1',
                category: 'chat',
                capabilities: { inboundText: true, outboundText: true, threads: true, idempotentDelivery: true },
              },
              status: { accepted: true },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<GatewayClassesPanel />)
    await waitFor(() => expect(screen.getByText('slack')).toBeInTheDocument())
    expect(screen.getByText('chat')).toBeInTheDocument()
    expect(screen.getByText('orka.gateway.v1')).toBeInTheDocument()
    expect(screen.getByText('threads')).toBeInTheDocument()
    // Column header + status badge both say "Accepted".
    expect(screen.getAllByText('Accepted')).toHaveLength(2)
  })
})
