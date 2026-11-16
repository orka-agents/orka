import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ListResponse } from '@/lib/list-api'
import type { Connection, ConnectionAuthorizeResponse, ConnectorProvider } from '@/lib/connectors'
import type { UsageOther, UsageReport, UsageWork } from '@/lib/usage'
import type { Gateway, GatewayBinding, GatewayDelivery, GatewayEvent } from '@/schemas/gateway'
import { isGatewayResourceReady } from '@/components/gateways/gateway-readiness'
import { handleDemoRequest } from '../server'
import { NOW } from '../shared'
import { TASKS } from '../world'

async function call<T>(path: string, init?: RequestInit & { json?: unknown }): Promise<{ status: number; body: T }> {
  const request = new Request(`http://localhost/api/v1${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json' },
    body: init?.json === undefined ? undefined : JSON.stringify(init.json),
  })
  const response = await handleDemoRequest(request)
  const text = await response.text()
  return { status: response.status, body: (text ? JSON.parse(text) : undefined) as T }
}

const get = <T,>(path: string) => call<T>(path).then((result) => {
  expect(result.status, path).toBe(200)
  return result.body
})

afterEach(() => {
  vi.useRealTimers()
})

describe('demo gateways', () => {
  it('serves ready and degraded gateways with bindings that resolve', async () => {
    const gateways = await get<ListResponse<Gateway>>('/gateways?namespace=default&limit=500')
    expect(gateways.items.map((g) => g.metadata.name)).toEqual(['slack-northwind', 'github-northwind', 'teams-support'])
    expect(gateways.items.filter(isGatewayResourceReady).map((g) => g.metadata.name)).toEqual(['slack-northwind', 'github-northwind'])

    const bindings = await get<ListResponse<GatewayBinding>>('/gatewaybindings?namespace=default')
    const names = new Set(gateways.items.map((g) => g.metadata.name))
    for (const binding of bindings.items) expect(names.has(binding.spec.gatewayRef.name)).toBe(true)
    const detail = await get<GatewayBinding>('/gatewaybindings/slack-platform-help?namespace=default')
    expect(detail.spec.senderPolicy?.allowedSenderIds?.length).toBeGreaterThan(0)

    expect((await call('/gateways/nope?namespace=default')).status).toBe(404)
    expect((await get<ListResponse<Gateway>>('/gateways?namespace=orka-system')).items).toEqual([])
  })

  it('links events to world tasks and filters the ledger by gateway and state', async () => {
    const events = await get<ListResponse<GatewayEvent>>('/gateway-events?namespace=default&limit=100')
    const taskNames = new Set(TASKS.map((task) => task.name))
    for (const event of events.items) if (event.taskName) expect(taskNames.has(event.taskName), event.taskName).toBe(true)
    expect(new Set(events.items.map((event) => event.state))).toEqual(
      new Set(['Accepted', 'Queued', 'Dispatching', 'TaskCreated', 'Completed', 'Rejected', 'DeadLettered', 'Expired']),
    )

    const slack = await get<ListResponse<GatewayEvent>>('/gateway-events?namespace=default&gateway=slack-northwind')
    expect(slack.items.every((event) => event.gatewayName === 'slack-northwind')).toBe(true)

    const firstPage = await get<ListResponse<GatewayEvent>>('/gateway-events?namespace=default&limit=4')
    expect(firstPage.items).toHaveLength(4)
    const second = await get<ListResponse<GatewayEvent>>(`/gateway-events?namespace=default&limit=4&continue=${firstPage.metadata?.continue}`)
    expect(second.items[0].id).toBe(events.items[4].id)
  })

  it('retries a dead-lettered delivery until it is delivered', async () => {
    const deliveries = await get<ListResponse<GatewayDelivery>>('/gateway-deliveries?namespace=default&state=DeadLettered')
    expect(deliveries.items).toHaveLength(1)
    const [stuck] = deliveries.items

    vi.useFakeTimers({ shouldAdvanceTime: true })
    const retried = await call<GatewayDelivery>(`/gateway-deliveries/${stuck.id}/retry?namespace=default`, { method: 'POST', json: {} })
    expect(retried.body.state).toBe('Pending')
    expect(retried.body.manualRetryCount).toBe(1)
    vi.advanceTimersByTime(3000)

    const after = await get<ListResponse<GatewayDelivery>>('/gateway-deliveries?namespace=default')
    const delivered = after.items.find((item) => item.id === stuck.id)
    expect(delivered?.state).toBe('Delivered')
    expect(delivered?.lastError).toBeUndefined()
    expect((await call(`/gateway-deliveries/${stuck.id}/retry?namespace=default`, { method: 'POST', json: {} })).status).toBe(409)
  })
})

describe('demo connectors', () => {
  it('links, upgrades, and disconnects an account through the consent callback', async () => {
    const providers = await get<{ items: ConnectorProvider[] }>('/connectors?namespace=default')
    expect(providers.items.map((p) => p.name)).toContain('linear')
    const before = await get<{ items: Connection[] }>('/connections?namespace=default')
    expect(before.items.some((c) => c.provider === 'linear')).toBe(false)

    const started = await call<ConnectionAuthorizeResponse>('/connections?namespace=default', { method: 'POST', json: { provider: 'linear', mode: 'readOnly' } })
    expect(started.body.connection.state).toBe('Pending')
    // The consent comes back to the Connectors page with the callback the page reads.
    const callback = new URL(started.body.authorizeURL!)
    const fragment = new URLSearchParams(callback.hash.slice(1))
    expect(callback.hash.startsWith('#/settings/connectors?')).toBe(true)
    expect(fragment.get('connection')).toBe(started.body.connection.name)
    const completion = fragment.get('completion')!

    const name = started.body.connection.name
    const linked = await call<Connection>(`/connections/${name}/complete?namespace=default`, { method: 'POST', json: { completion } })
    expect(linked.body).toMatchObject({ provider: 'linear', mode: 'readOnly', state: 'Ready', ready: true })

    // Writes need a fresh consent; dropping them does not.
    const upgrade = await call<ConnectionAuthorizeResponse>(`/connections/${name}?namespace=default`, { method: 'PUT', json: { mode: 'readWrite' } })
    expect(upgrade.body.authorizeURL).toBeTruthy()
    const github = (await get<{ items: Connection[] }>('/connections?namespace=default')).items.find((c) => c.provider === 'github')!
    const downgrade = await call<ConnectionAuthorizeResponse>(`/connections/${github.name}?namespace=default`, { method: 'PUT', json: { mode: 'readOnly' } })
    expect(downgrade.body.authorizeURL).toBeUndefined()
    expect(downgrade.body.connection.mode).toBe('readOnly')

    vi.useFakeTimers({ shouldAdvanceTime: true })
    expect((await call(`/connections/${name}?namespace=default`, { method: 'DELETE' })).status).toBe(204)
    const disconnecting = await get<{ items: Connection[] }>('/connections?namespace=default')
    expect(disconnecting.items.find((c) => c.name === name)?.deleting).toBe(true)
    vi.advanceTimersByTime(4000)
    const gone = await get<{ items: Connection[] }>('/connections?namespace=default')
    expect(gone.items.some((c) => c.name === name)).toBe(false)
  })

  it('refuses a provider that is not ready', async () => {
    const result = await call('/connections?namespace=default', { method: 'POST', json: { provider: 'jira', mode: 'readOnly' } })
    expect(result.status).toBe(409)
  })
})

describe('demo usage', () => {
  const day = (offsetDays: number) => new Date(NOW + offsetDays * 86_400_000).toISOString().slice(0, 10)
  const range = `from=${day(-30)}&until=${day(1)}`

  it('reports a paged cohort with consistent PR outcomes', async () => {
    const report = await get<UsageReport>(`/usage?namespace=default&${range}&offset=0&limit=25`)
    expect(report.page.total).toBeGreaterThan(25)
    expect(report.works).toHaveLength(25)
    expect(report.summary.workRequests).toBe(report.page.total)
    expect(report.summary.prsMerged).toBeGreaterThan(0)
    expect(report.summary.tokensPerPRMerged).toBeCloseTo(report.summary.totalTokens / report.summary.prsMerged)
    expect(report.teams).toEqual([{ namespace: 'default', summary: report.summary }])
    expect(report.otherWork.map((group) => group.category)).toEqual(['review_only', 'other_requests', 'unassociated'])

    const next = await get<UsageReport>(`/usage?namespace=default&${range}&offset=25&limit=25&asOf=${report.selection.asOf}`)
    expect(next.works[0].id).not.toBe(report.works[0].id)
    expect(next.summary).toEqual(report.summary)
  })

  it('drills into a work request with its tasks, PRs, and models', async () => {
    const report = await get<UsageReport>(`/usage?namespace=default&${range}&limit=100`)
    const current = report.works.find((work) => work.repository === 'northwind-labs/checkout-service' && work.number === 2214)!
    const params = `namespace=default&from=${report.selection.from}&until=${report.selection.until}&asOf=${report.selection.asOf}`
    const { work } = await get<{ work: UsageWork }>(`/usage/work/${current.id}?${params}`)
    const seed = TASKS.find((task) => task.name === 'fix-cart-rounding-bug-x9m4d')!
    expect(work.tasks?.map((task) => task.taskName)).toEqual([seed.name])
    expect(work.tasks?.[0].usage.inputTokens).toBe(seed.inputTokens)
    expect(work.models).toEqual(['gpt-5'])

    // The gateway review of checkout-service#1479 is shared usage on the request that opened it.
    const outbox = report.works.find((item) => item.number === 2201)!
    const detail = await get<{ work: UsageWork }>(`/usage/work/${outbox.id}?${params}`)
    expect(detail.work.pullRequests?.map((pr) => pr.number)).toEqual([1479])
    expect(detail.work.tasks?.find((task) => task.taskName === 'review-pr-1479-b7n2k')?.shared).toBe(true)
  })

  it('pages other usage and applies filters', async () => {
    const other = await get<{ otherWork: UsageOther }>(`/usage/other/unassociated?namespace=default&${range}&offset=0&limit=25`)
    expect(other.otherWork.tasks?.length).toBe(Math.min(25, other.otherWork.taskCount))
    expect(other.otherWork.tasks?.some((task) => task.taskName === 'checkout-latency-investigation-7k2qp')).toBe(true)

    const pullRequests = await get<UsageReport>(`/usage?namespace=default&${range}&kind=pull_request`)
    expect(pullRequests.works).toEqual([])
    expect(pullRequests.otherWork[0].taskCount).toBeGreaterThan(0)

    const sonnet = await get<UsageReport>(`/usage?namespace=default&${range}&model=claude-sonnet-4-5`)
    const all = await get<UsageReport>(`/usage?namespace=default&${range}`)
    expect(sonnet.page.total).toBeGreaterThan(0)
    expect(sonnet.page.total).toBeLessThan(all.page.total)

    const repo = await get<UsageReport>(`/usage?namespace=default&${range}&repository=northwind-labs/inventory-api`)
    expect(repo.page.total).toBeGreaterThan(0)

    expect((await call('/usage?namespace=default&kind=epic')).status).toBe(400)
    expect((await call(`/usage?namespace=default&from=${day(1)}&until=${day(-1)}`)).status).toBe(400)
    expect((await call('/usage/other/everything?namespace=default')).status).toBe(404)

    const elsewhere = await get<UsageReport>(`/usage?namespace=orka-system&${range}`)
    expect(elsewhere.summary.workRequests).toBe(0)
    expect(elsewhere.summary.completeness).toBe('unavailable')
  })
})
