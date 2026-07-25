import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useUIStore } from '@/stores/ui'
import { useGatewayBinding, useGatewayDeliveries, useGatewayEvents, useGatewayLedgerPagination, useGateways } from './use-gateways'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
})

describe('gateway ledger hooks', () => {
  it('fetches only the requested event page', async () => {
    const requests: URL[] = []
    server.use(http.get('/api/v1/gateway-events', ({ request }) => {
      requests.push(new URL(request.url))
      return HttpResponse.json({ items: [], metadata: { continue: 'next-events' } })
    }))

    const { result } = renderHook(
      () => useGatewayEvents({ gateway: 'chat', state: 'Queued', limit: '25', continue: 'event-cursor' }),
      { wrapper: createWrapper() },
    )
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(requests).toHaveLength(1)
    expect(requests[0].searchParams.get('namespace')).toBe('default')
    expect(requests[0].searchParams.get('gateway')).toBe('chat')
    expect(requests[0].searchParams.get('state')).toBe('Queued')
    expect(requests[0].searchParams.get('limit')).toBe('25')
    expect(requests[0].searchParams.get('continue')).toBe('event-cursor')
    expect(result.current.data?.metadata?.continue).toBe('next-events')
  })

  it('uses a bounded default delivery page', async () => {
    const requests: URL[] = []
    server.use(http.get('/api/v1/gateway-deliveries', ({ request }) => {
      requests.push(new URL(request.url))
      return HttpResponse.json({ items: [], metadata: {} })
    }))

    const { result } = renderHook(() => useGatewayDeliveries(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(requests).toHaveLength(1)
    expect(requests[0].searchParams.get('limit')).toBe('100')
  })

  it('fetches a GatewayBinding detail in the active namespace', async () => {
    const requests: URL[] = []
    server.use(http.get('/api/v1/gatewaybindings/:name', ({ request, params }) => {
      requests.push(new URL(request.url))
      return HttpResponse.json({
        metadata: { name: params.name },
        spec: {
          gatewayRef: { name: 'chat' },
          agentRef: { name: 'operator' },
          match: { accountId: 'acme', contextId: 'support' },
        },
      })
    }))

    const { result } = renderHook(() => useGatewayBinding('support-binding'), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(requests).toHaveLength(1)
    expect(requests[0].pathname).toBe('/api/v1/gatewaybindings/support-binding')
    expect(requests[0].searchParams.get('namespace')).toBe('default')
    expect(result.current.data?.spec.agentRef.name).toBe('operator')
  })

  it('resets ledger pagination for a newly selected namespace', async () => {
    const { result } = renderHook(() => useGatewayLedgerPagination('events'), { wrapper: createWrapper() })

    act(() => result.current.next('default-page-2'))
    expect(result.current.page).toBe(2)
    expect(result.current.cursor).toBe('default-page-2')

    act(() => useUIStore.setState({ namespace: 'other' }))
    await waitFor(() => expect(result.current.page).toBe(1))
    expect(result.current.cursor).toBe('')
    expect(result.current.hasPrevious).toBe(false)

    act(() => useUIStore.setState({ namespace: 'default' }))
    await waitFor(() => expect(result.current.page).toBe(1))
    expect(result.current.cursor).toBe('')
    expect(result.current.hasPrevious).toBe(false)
  })

  it('loads Gateway inventory beyond twenty server pages', async () => {
    const requests: string[] = []
    server.use(http.get('/api/v1/gateways', ({ request }) => {
      const cursor = new URL(request.url).searchParams.get('continue') ?? ''
      requests.push(cursor)
      const page = cursor === '' ? 0 : Number(cursor)
      return HttpResponse.json({
        items: [{
          metadata: { name: `gateway-${page}`, namespace: 'default' },
          spec: { gatewayClassName: 'generic', adapter: { endpoint: 'https://adapter.example' } },
        }],
        metadata: page < 21 ? { continue: String(page + 1) } : {},
      })
    }))

    const { result } = renderHook(() => useGateways(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(requests).toHaveLength(22)
    expect(result.current.data?.items).toHaveLength(22)
    expect(result.current.data?.items.at(-1)?.metadata.name).toBe('gateway-21')
  })
})
