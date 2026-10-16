import { http, HttpResponse, type RequestHandler } from 'msw'
import type { GatewayDelivery } from '@/schemas/gateway'
import { api, apiError, inDemoNamespace, notFound, query } from '../shared'
import { gatewayBindings, gatewayDeliveries, gatewayEvents, gateways } from '../data/gateways'
import { connectorHandlers } from './connectors'
import { usageHandlers } from './usage'

const deliveries: GatewayDelivery[] = gatewayDeliveries.map((delivery) => ({ ...delivery }))

/**
 * One page of a ledger list, filtered by gateway and state. The continue token
 * is the next offset, like the controller's opaque cursor.
 */
function ledgerPage<T extends { gatewayName: string; state: string }>(request: Request, items: T[]) {
  if (!inDemoNamespace(request)) return HttpResponse.json({ items: [], metadata: {} })
  const gateway = query(request, 'gateway')
  const state = query(request, 'state')
  const filtered = items.filter((item) => (!gateway || item.gatewayName === gateway) && (!state || item.state === state))
  const limit = Math.max(1, Number(query(request, 'limit') ?? 100) || 100)
  const offset = Math.max(0, Number(query(request, 'continue') ?? 0) || 0)
  const next = offset + limit
  return HttpResponse.json({
    items: filtered.slice(offset, next),
    metadata: next < filtered.length ? { continue: String(next), remainingItemCount: filtered.length - next } : {},
  })
}

const byUpdated = (a: GatewayDelivery, b: GatewayDelivery) => b.updatedAt.localeCompare(a.updatedAt)

/** A manual retry goes out on the next adapter pass and lands a moment later. */
function retry(delivery: GatewayDelivery) {
  const now = new Date().toISOString()
  Object.assign(delivery, { state: 'Pending', manualRetryCount: delivery.manualRetryCount + 1, nextAttemptAt: now, updatedAt: now })
  setTimeout(() => {
    const at = new Date().toISOString()
    Object.assign(delivery, {
      state: 'Delivered',
      attemptCount: delivery.attemptCount + 1,
      lastError: undefined,
      providerMessageId: delivery.replyTarget.startsWith('github:')
        ? `github:pullrequestreview/${2384700000 + Math.floor(Math.random() * 99999)}`
        : `${delivery.replyTarget}/${(Date.now() / 1000).toFixed(6)}`,
      updatedAt: at,
      deliveredAt: at,
    })
  }, 2500)
}

/** Gateways, bindings, the event ledger, and the delivery outbox; plus connectors and usage. */
export const gatewayHandlers: RequestHandler[] = [
  http.get(api('/gateways'), ({ request }) => HttpResponse.json({ items: inDemoNamespace(request) ? gateways : [], metadata: {} })),

  http.get(api('/gateways/:name'), ({ request, params }) => {
    const gateway = inDemoNamespace(request) ? gateways.find((item) => item.metadata.name === params.name) : undefined
    return gateway ? HttpResponse.json(gateway) : notFound(`gateways.gateway.orka.ai "${String(params.name)}"`)
  }),

  http.get(api('/gatewaybindings'), ({ request }) =>
    HttpResponse.json({ items: inDemoNamespace(request) ? gatewayBindings : [], metadata: {} })),

  http.get(api('/gatewaybindings/:name'), ({ request, params }) => {
    const binding = inDemoNamespace(request) ? gatewayBindings.find((item) => item.metadata.name === params.name) : undefined
    return binding ? HttpResponse.json(binding) : notFound(`gatewaybindings.gateway.orka.ai "${String(params.name)}"`)
  }),

  http.get(api('/gateway-events'), ({ request }) => ledgerPage(request, gatewayEvents)),

  http.get(api('/gateway-deliveries'), ({ request }) => ledgerPage(request, [...deliveries].sort(byUpdated))),

  http.post(api('/gateway-deliveries/:id/retry'), ({ request, params }) => {
    const delivery = inDemoNamespace(request) ? deliveries.find((item) => item.id === params.id) : undefined
    if (!delivery) return notFound(`gateway delivery ${String(params.id)}`)
    if (delivery.state !== 'DeadLettered' && delivery.state !== 'Failed') {
      return apiError(409, `gateway delivery is ${delivery.state}; only Failed or DeadLettered deliveries can be retried`)
    }
    retry(delivery)
    return HttpResponse.json(delivery)
  }),

  ...connectorHandlers,
  ...usageHandlers,
]
