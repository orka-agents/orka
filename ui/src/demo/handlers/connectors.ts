import { http, HttpResponse } from 'msw'
import type { Connection } from '@/lib/connectors'
import { api, apiError, DEMO_NAMESPACE, digest, inDemoNamespace, noContent, notFound } from '../shared'
import { connectionName, connectorProviders, grantedScopes, seedConnections } from '../data/connectors'

// Linking an account normally sends the browser to the provider's consent
// page, which redirects back with a one-time completion token. The demo skips
// the provider: the consent URL points straight back at the Connectors page
// with that callback, and the page finishes the link as it would for real.
// The full-page round trip resets in-memory state, so links persist in
// sessionStorage for the tab.

interface StoredConnection extends Connection {
  /** mode the pending consent grants */
  pendingMode?: string
  /** when a requested disconnect finishes (epoch ms) */
  removeAt?: number
}

const STORAGE_KEY = 'orka-demo-connections'

function load(): StoredConnection[] {
  try {
    const saved = sessionStorage.getItem(STORAGE_KEY)
    if (saved) return JSON.parse(saved) as StoredConnection[]
  } catch {
    // Storage unavailable: start from the seed.
  }
  return seedConnections()
}

let connections = load()

function save() {
  try {
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(connections))
  } catch {
    // Links still work for this page load.
  }
}

/** Drops links whose disconnect has finished. */
function current(): StoredConnection[] {
  const before = connections.length
  connections = connections.filter((c) => !c.removeAt || c.removeAt > Date.now())
  if (connections.length !== before) save()
  return connections
}

/** The Connection as the API returns it, without the demo's bookkeeping. */
function publicView(stored: StoredConnection): Connection {
  const connection = { ...stored }
  delete connection.pendingMode
  delete connection.removeAt
  return connection
}

const displayName = (provider: string) => connectorProviders.find((p) => p.name === provider)?.displayName ?? provider

/** Where the provider would send the browser after consent: back to this page with the callback. */
function consentURL(connection: StoredConnection): string {
  const callback = new URLSearchParams({
    status: 'pending',
    connection: connection.name,
    namespace: connection.namespace,
    completion: `demo-${digest(`${connection.name}:${Date.now()}`, 24)}`,
  })
  const url = new URL(window.location.href)
  // A fresh query forces a real page load, as returning from a provider would.
  url.search = `?consent=${Date.now().toString(36)}`
  url.hash = `/settings/connectors?${callback}`
  return url.toString()
}

function startConsent(connection: StoredConnection, mode: string) {
  connection.pendingMode = mode
  save()
  return HttpResponse.json({ connection: publicView(connection), authorizeURL: consentURL(connection) })
}

const find = (name: string) => current().find((c) => c.name === name)

/** ConnectorProviders and the visitor's linked accounts. */
export const connectorHandlers = [
  http.get(api('/connectors'), ({ request }) =>
    HttpResponse.json({ items: inDemoNamespace(request) ? connectorProviders : [] })),

  http.get(api('/connections'), ({ request }) =>
    HttpResponse.json({ items: inDemoNamespace(request) ? current().map(publicView) : [] })),

  http.post(api('/connections'), async ({ request }) => {
    if (!inDemoNamespace(request)) return apiError(404, 'connector provider not found')
    const { provider, mode = 'readOnly' } = (await request.json()) as { provider?: string; mode?: string }
    const configured = connectorProviders.find((p) => p.name === provider)
    if (!provider || !configured) return apiError(404, `connector provider ${provider ?? ''} not found`)
    if (!configured.ready) return apiError(409, `connector provider ${provider} is not accepted right now; retry later`)
    const existing = current().find((c) => c.provider === provider && !c.deleting)
    if (existing?.ready && existing.mode === mode) return HttpResponse.json({ connection: publicView(existing) })
    if (existing) return startConsent(existing, mode)
    const connection: StoredConnection = {
      name: connectionName(provider),
      namespace: DEMO_NAMESPACE,
      provider,
      mode,
      state: 'Pending',
      ready: false,
      message: `Waiting for consent at ${displayName(provider)}.`,
    }
    connections.push(connection)
    return startConsent(connection, mode)
  }),

  http.put(api('/connections/:name'), async ({ request, params }) => {
    const connection = find(String(params.name))
    if (!connection || !inDemoNamespace(request)) return notFound(`connection ${String(params.name)}`)
    const { mode } = (await request.json()) as { mode?: string }
    if (mode !== 'readOnly' && mode !== 'readWrite') return apiError(400, 'mode must be readOnly or readWrite')
    // Write access needs a fresh consent for the write scopes; dropping it does not.
    if (mode === 'readWrite' && connection.mode !== 'readWrite') return startConsent(connection, mode)
    connection.mode = mode
    connection.grantedScopes = grantedScopes(connection.provider, mode)
    save()
    return HttpResponse.json({ connection: publicView(connection) })
  }),

  http.post(api('/connections/:name/authorize'), ({ request, params }) => {
    const connection = find(String(params.name))
    if (!connection || !inDemoNamespace(request)) return notFound(`connection ${String(params.name)}`)
    return startConsent(connection, connection.mode)
  }),

  http.post(api('/connections/:name/complete'), async ({ request, params }) => {
    const name = String(params.name)
    const { completion } = (await request.json().catch(() => ({}))) as { completion?: string }
    if (!completion?.startsWith('demo-')) return apiError(409, 'the consent expired or was superseded')
    let connection = find(name)
    if (!connection) {
      // Storage was unavailable across the round trip: rebuild the link from its name.
      const provider = connectorProviders.find((p) => connectionName(p.name) === name)
      if (!provider || !inDemoNamespace(request)) return notFound(`connection ${name}`)
      connection = { name, namespace: DEMO_NAMESPACE, provider: provider.name, mode: 'readOnly', state: 'Pending', ready: false }
      connections.push(connection)
    }
    const mode = connection.pendingMode ?? connection.mode
    Object.assign(connection, {
      mode,
      state: 'Ready',
      ready: true,
      message: undefined,
      pendingMode: undefined,
      grantedScopes: grantedScopes(connection.provider, mode),
      linkedAt: connection.linkedAt ?? new Date().toISOString(),
      lastRefreshTime: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 8 * 3600_000).toISOString(),
    })
    save()
    return HttpResponse.json(publicView(connection))
  }),

  http.delete(api('/connections/:name'), ({ request, params }) => {
    const connection = find(String(params.name))
    if (!connection || !inDemoNamespace(request)) return notFound(`connection ${String(params.name)}`)
    // The finalizer revokes the provider tokens before the link disappears.
    Object.assign(connection, { deleting: true, ready: false, removeAt: Date.now() + 3500 })
    save()
    return noContent()
  }),
]
