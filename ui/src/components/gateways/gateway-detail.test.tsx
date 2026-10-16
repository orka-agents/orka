import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen } from '@/test/test-utils'
import { server } from '@/test/mocks/server'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { GatewayDetail } from './gateway-detail'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, className }: { children: ReactNode; to: string; className?: string }) => <a href={to} className={className}>{children}</a>,
  }
})

describe('GatewayDetail', () => {
  beforeEach(() => {
    useAuthStore.setState({ ['token']: 'test-token' })
    useUIStore.setState({ namespace: 'default' })
    document.title = 'Orka'
    server.use(
      http.get('/api/v1/gatewaybindings', () => HttpResponse.json({ items: [] })),
      http.get('/api/v1/gateway-events', () => HttpResponse.json({ items: [] })),
      http.get('/api/v1/gateway-deliveries', () => HttpResponse.json({ items: [] })),
    )
  })

  it('keeps every ledger tab reachable on narrow screens', async () => {
    server.use(http.get('/api/v1/gateways/chat', () => HttpResponse.json({
      metadata: { name: 'chat', namespace: 'default', generation: 1 },
      spec: { gatewayClassName: 'generic-chat', adapter: { endpoint: 'https://adapter.example' } },
      status: { ready: true, observedGeneration: 1 },
    })))

    render(<GatewayDetail name="chat" />)

    const bindingsTab = await screen.findByRole('tab', { name: 'Bindings' })
    // The tab strip scrolls horizontally instead of pushing tabs past the viewport.
    expect(screen.getByRole('tablist').parentElement).toHaveClass('overflow-x-auto')
    expect(bindingsTab).toBeInTheDocument()
  })

  it('renders a titled access-error page with a way back on 403', async () => {
    server.use(http.get('/api/v1/gateways/chat', () => HttpResponse.json(
      { error: { code: 403, message: 'not authorized' } },
      { status: 403 },
    )))

    render(<GatewayDetail name="chat" />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Could not load Gateway' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('Not authorized to view Gateway chat')
    expect(screen.getByRole('link', { name: 'Back to Gateways' })).toHaveAttribute('href', '/gateways')
    expect(document.title).toBe('Could not load Gateway · Orka')
    expect(screen.queryByRole('heading', { name: 'Gateway not found' })).not.toBeInTheDocument()
  })
})
