import { afterEach, describe, expect, it, vi } from 'vitest'
import { z } from 'zod'
import {
  monitorActionSchema,
  monitorCommandSchema,
  monitorImplementationJobSchema,
  monitorItemSchema,
  monitorMutationSchema,
  monitorRunSchema,
  monitorWorkActionSchema,
  repositoryMonitorSchema,
} from '@/schemas/monitor'
import {
  droppedFindingSchema,
  patchProposalSchema,
  repositoryScanSchema,
  reviewSliceSchema,
  scanRunSchema,
  securityFindingSchema,
  threatModelSchema,
} from '@/schemas/security'
import { statusProblem } from '@/components/common/status-problem'
import { items, workActions } from '../data/monitors'
import { findings } from '../data/security'
import { handleDemoRequest } from '../server'
import { TASKS } from '../world'
import { createDemoTask } from './tasks'

// Monitor commands hand work to the task domain; stub it so these tests cover
// only this domain's side of the contract.
vi.mock('./tasks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./tasks')>()),
  createDemoTask: vi.fn(() => 'review-pr-1482-demo1'),
}))

const call = (path: string, init?: RequestInit) => handleDemoRequest(new Request(`http://localhost/api/v1${path}`, init))
const json = async (path: string, init?: RequestInit) => (await call(path, init)).json()
const post = (path: string, body?: unknown) =>
  call(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body), headers: { 'Content-Type': 'application/json' } })
const listOf = <T extends z.ZodTypeAny>(schema: T) => z.object({ items: z.array(schema), metadata: z.object({ continue: z.string().optional() }).optional() })

/** Moves the demo clock forward so timed work (scans, validation, patches) settles. */
function advance(ms: number) {
  const base = Date.now()
  vi.spyOn(Date, 'now').mockReturnValue(base + ms)
}

afterEach(() => {
  vi.restoreAllMocks()
})

const taskNames = new Set(TASKS.map((task) => task.name))

describe('demo security repositories', () => {
  it('lists repositories with derived status that parses', async () => {
    const { items: repos } = listOf(repositoryScanSchema).parse(await json('/security/repositories?namespace=default'))
    expect(repos.map((repo) => repo.metadata.name)).toEqual(['checkout-service', 'inventory-api', 'web-storefront', 'platform-infra'])

    const checkout = repos[0]
    expect(checkout.status?.phase).toBe('Ready')
    expect(checkout.status?.findingCounts?.critical).toBeGreaterThan(0)
    expect(checkout.status?.findingCounts?.total).toBe(
      (checkout.status?.findingCounts?.critical ?? 0) + (checkout.status?.findingCounts?.high ?? 0) +
      (checkout.status?.findingCounts?.medium ?? 0) + (checkout.status?.findingCounts?.low ?? 0),
    )
    expect(repos[2].status?.phase).toBe('Scanning')
    expect(statusProblem(repos[3].status)?.reason).toBe('CredentialUnavailable')
  })

  it('serves repository detail, slices, scans, dropped findings, and the threat model', async () => {
    const repo = repositoryScanSchema.parse(await json('/security/repositories/checkout-service?namespace=default'))
    expect(repo.spec.analysisAgentRef.name).toBe('security-auditor')
    expect(listOf(reviewSliceSchema).parse(await json('/security/repositories/checkout-service/slices?namespace=default')).items.length).toBeGreaterThan(3)
    const scans = listOf(scanRunSchema).parse(await json('/security/repositories/checkout-service/scans?namespace=default')).items
    expect(scans[0].id).toBe(repo.status?.lastScanID)
    expect(taskNames.has(scans[0].taskName)).toBe(true)
    const dropped = listOf(droppedFindingSchema).parse(await json(`/security/repositories/checkout-service/dropped-findings?namespace=default&scanRunID=${repo.status?.lastScanID}`)).items
    expect(dropped.length).toBeGreaterThan(0)
    expect(dropped.every((d) => d.scanRunID === repo.status?.lastScanID)).toBe(true)
    const model = threatModelSchema.parse(await json('/security/repositories/checkout-service/threat-model?namespace=default'))
    expect(model.content).toContain('# checkout-service threat model')
    expect((await call('/security/repositories/platform-infra/threat-model?namespace=default')).status).toBe(404)
  })

  it('keeps other namespaces empty', async () => {
    expect((await json('/security/repositories?namespace=orka-system')).items).toEqual([])
    expect((await call('/security/repositories/checkout-service?namespace=orka-system')).status).toBe(404)
  })

  it('saves threat model edits with optimistic concurrency', async () => {
    const put = (expectedVersion: number) =>
      call('/security/repositories/inventory-api/threat-model?namespace=default', {
        method: 'PUT',
        body: JSON.stringify({ content: '# edited', source: 'edited', expectedVersion }),
      })
    expect((await put(1)).status).toBe(409)
    const saved = threatModelSchema.parse(await (await put(2)).json())
    expect(saved).toMatchObject({ version: 3, content: '# edited', source: 'edited' })
  })

  it('runs a manual scan to completion and refuses overlapping scans', async () => {
    const response = await post('/security/repositories/checkout-service/scans?namespace=default')
    expect(response.status).toBe(201)
    const run = scanRunSchema.parse(await response.json())
    expect(run).toMatchObject({ mode: 'manual', phase: 'running' })
    expect((await post('/security/repositories/checkout-service/scans?namespace=default')).status).toBe(409)
    expect((await post('/security/repositories/platform-infra/scans?namespace=default')).status).toBe(409)

    advance(60_000)
    const scans = listOf(scanRunSchema).parse(await json('/security/repositories/checkout-service/scans?namespace=default')).items
    expect(scans[0]).toMatchObject({ id: run.id, phase: 'succeeded' })
  })

  it('registers a new repository and starts its first scan', async () => {
    const response = await post('/security/repositories', {
      name: 'payments-gateway',
      namespace: 'default',
      spec: { repoURL: 'https://github.com/northwind-labs/payments-gateway', branch: 'main', validationMode: 'full', analysisAgentRef: { name: 'security-auditor' } },
    })
    expect(response.status).toBe(201)
    expect(repositoryScanSchema.parse(await response.json()).status?.phase).toBe('Scanning')
    expect((await post('/security/repositories', { name: 'payments-gateway', spec: { repoURL: 'x', analysisAgentRef: { name: 'a' } } })).status).toBe(409)
  })
})

describe('demo security findings', () => {
  const findingsOf = async (repo: string, params = '') =>
    listOf(securityFindingSchema).parse(await json(`/security/repositories/${repo}/findings?namespace=default${params}`))

  it('points artifact evidence at artifacts its task serves', async () => {
    const refs = findings.flatMap((finding) => (finding.evidence ?? []).filter((evidence) => evidence.kind === 'artifact'))
    expect(refs.length).toBeGreaterThan(0)
    for (const ref of refs) {
      const response = await call(`/tasks/${encodeURIComponent(ref.taskName ?? '')}/artifacts/${encodeURIComponent(ref.name ?? '')}?namespace=default`)
      expect(response.status, `${ref.taskName}/${ref.name}`).toBe(200)
    }
  })

  it('pages findings with the cursor the dashboard follows', async () => {
    const all = await findingsOf('checkout-service')
    const first = await findingsOf('checkout-service', '&limit=3')
    expect(first.items).toHaveLength(3)
    expect(first.metadata?.continue).toBe('3')
    const second = await findingsOf('checkout-service', `&limit=3&cursor=${first.metadata?.continue}`)
    expect([...first.items, ...second.items].map((f) => f.id)).toEqual(all.items.slice(0, 6).map((f) => f.id))
  })

  it('recommends open, not-disproven findings by severity', async () => {
    const { items: recommended } = await findingsOf('checkout-service', '&recommended=true&limit=5')
    expect(recommended[0].severity).toBe('critical')
    expect(recommended.some((f) => f.validationStatus === 'failed' || f.state === 'dismissed')).toBe(false)
    const { items: high } = await findingsOf('checkout-service', '&severity=high')
    expect(high.every((f) => f.severity === 'high')).toBe(true)
  })

  it('dismisses and reopens a finding, updating repository counts', async () => {
    const { items: [finding] } = await findingsOf('web-storefront', '&severity=high')
    const before = repositoryScanSchema.parse(await json('/security/repositories/web-storefront?namespace=default'))
    expect((await post(`/security/findings/${finding.id}/dismiss?namespace=default`)).status).toBe(204)
    expect(securityFindingSchema.parse(await json(`/security/findings/${finding.id}?namespace=default`)).state).toBe('dismissed')
    const after = repositoryScanSchema.parse(await json('/security/repositories/web-storefront?namespace=default'))
    expect(after.status?.findingCounts?.high).toBe((before.status?.findingCounts?.high ?? 0) - 1)
    expect((await post(`/security/findings/${finding.id}/reopen?namespace=default`)).status).toBe(204)
    expect(securityFindingSchema.parse(await json(`/security/findings/${finding.id}?namespace=default`)).state).toBe('open')
  })

  it('validates a finding, then generates a patch and opens a pull request', async () => {
    const { items: [finding] } = await findingsOf('inventory-api', '&state=open&validationStatus=unvalidated')
    expect((await post(`/security/findings/${finding.id}/validate?namespace=default`)).status).toBe(202)
    expect(securityFindingSchema.parse(await json(`/security/findings/${finding.id}?namespace=default`)).validationStatus).toBe('pending')

    const patchResponse = await post(`/security/findings/${finding.id}/patch?namespace=default`)
    expect(patchResponse.status).toBe(201)
    expect(patchProposalSchema.parse(await patchResponse.json()).status).toBe('running')
    expect((await post(`/security/findings/${finding.id}/pull-request?namespace=default`)).status).toBe(409)

    advance(90_000)
    const validated = securityFindingSchema.parse(await json(`/security/findings/${finding.id}?namespace=default`))
    expect(validated.validationStatus).toBe('validated')
    expect(JSON.parse(validated.validationJSON ?? '{}').validation_steps.length).toBeGreaterThan(0)
    const proposals = listOf(patchProposalSchema).parse(await json(`/security/findings/${finding.id}/patches?namespace=default`)).items
    expect(proposals[0].status).toBe('succeeded')

    const prResponse = await post(`/security/findings/${finding.id}/pull-request?namespace=default`)
    expect(prResponse.status).toBe(201)
    const pr = await prResponse.json()
    expect(pr).toMatchObject({ status: 'pr_opened', prURL: expect.stringContaining('/inventory-api/pull/') })
    expect(securityFindingSchema.parse(await json(`/security/findings/${finding.id}?namespace=default`))).toMatchObject({ state: 'pr_open', prNumber: pr.prNumber })
  })

  it('opens a pull request from an already successful patch', async () => {
    const { items: [finding] } = await findingsOf('checkout-service', '&state=patch_ready')
    const response = await post(`/security/findings/${finding.id}/pull-request?namespace=default`)
    expect(response.status).toBe(201)
    expect((await response.json()).prNumber).toBeGreaterThan(1486)
  })
})

describe('demo repository monitors', () => {
  it('lists monitors and their detail collections with valid shapes', async () => {
    const { items: monitors } = listOf(repositoryMonitorSchema).parse(await json('/monitors/repositories?namespace=default'))
    expect(monitors.map((m) => m.metadata.name)).toEqual(['checkout-service', 'inventory-api', 'web-storefront', 'platform-infra'])
    expect(monitors[0].status?.lastRunID).toMatch(/^run_checkout_/)
    expect(monitors[3].status?.phase).toBe('Suspended')

    repositoryMonitorSchema.parse(await json('/monitors/repositories/checkout-service?namespace=default'))
    expect(listOf(monitorRunSchema).parse(await json('/monitors/repositories/checkout-service/runs?namespace=default')).items.length).toBeGreaterThan(2)
    const prs = listOf(monitorItemSchema).parse(await json('/monitors/repositories/checkout-service/items?namespace=default&kind=pull_request')).items
    expect(prs.every((i) => i.kind === 'pull_request' && i.headSHA)).toBe(true)
    const issues = listOf(monitorItemSchema).parse(await json('/monitors/repositories/checkout-service/items?namespace=default&kind=issue')).items
    expect(issues.every((i) => i.kind === 'issue')).toBe(true)
    for (const [path, schema] of [
      ['actions', monitorActionSchema],
      ['commands', monitorCommandSchema],
      ['work-actions', monitorWorkActionSchema],
      ['implementation-jobs', monitorImplementationJobSchema],
      ['mutations', monitorMutationSchema],
    ] as const) {
      const entries = listOf(schema).parse(await json(`/monitors/${path}?namespace=default&name=checkout-service`)).items
      expect(entries.length, path).toBeGreaterThan(0)
      expect(entries.every((entry: { monitorName: string }) => entry.monitorName === 'checkout-service'), path).toBe(true)
    }
    expect((await json('/monitors/repositories?namespace=orka-system')).items).toEqual([])
  })

  it('references only tasks that exist in the demo world', () => {
    const referenced = [
      ...items.map((i) => i.lastActionTaskName),
      ...workActions.map((w) => w.taskName),
    ].filter((name): name is string => Boolean(name))
    expect(referenced.filter((name) => !taskNames.has(name))).toEqual([])
  })

  it('starts runs and refuses them on suspended monitors', async () => {
    const response = await post('/monitors/repositories/web-storefront/runs?namespace=default', {})
    expect(response.status).toBe(201)
    expect(monitorRunSchema.parse(await response.json())).toMatchObject({ trigger: 'manual', phase: 'running' })
    expect((await post('/monitors/repositories/platform-infra/runs?namespace=default', {})).status).toBe(409)
    advance(30_000)
    const runs = listOf(monitorRunSchema).parse(await json('/monitors/repositories/web-storefront/runs?namespace=default')).items
    expect(runs[0].phase).toBe('succeeded')
  })

  it('queues a review command that creates a task and finishes', async () => {
    const pr = monitorItemSchema.parse(listOf(monitorItemSchema).parse(await json('/monitors/repositories/checkout-service/items?namespace=default&kind=pull_request')).items.find((i) => i.number === 1482))
    const response = await post('/monitors/repositories/checkout-service/commands?namespace=default', { kind: 'pull_request', number: 1482, intent: 'review', targetSHA: pr.headSHA })
    expect(response.status).toBe(201)
    const command = monitorCommandSchema.parse(await response.json())
    expect(command).toMatchObject({ status: 'accepted', intent: 'review', source: 'api' })
    expect(createDemoTask).toHaveBeenCalledWith(expect.objectContaining({ agent: 'reviewer', repo: 'checkout', createdBy: 'monitor' }))

    const queued = listOf(monitorWorkActionSchema).parse(await json('/monitors/work-actions?namespace=default&name=checkout-service')).items[0]
    expect(queued).toMatchObject({ commandEventID: command.id, status: 'queued', taskName: 'review-pr-1482-demo1' })

    advance(60_000)
    const done = listOf(monitorWorkActionSchema).parse(await json('/monitors/work-actions?namespace=default&name=checkout-service')).items[0]
    expect(done.status).toBe('succeeded')
    const commands = listOf(monitorCommandSchema).parse(await json('/monitors/commands?namespace=default&name=checkout-service')).items
    expect(commands[0]).toMatchObject({ id: command.id, status: 'processed' })
    const prs = listOf(monitorItemSchema).parse(await json('/monitors/repositories/checkout-service/items?namespace=default&kind=pull_request')).items
    expect(prs.find((i) => i.number === 1482)?.lastVerdict).toBe('passed')
  })

  const issueCommand = (number: number, intent: string) =>
    post('/monitors/repositories/checkout-service/commands?namespace=default', { kind: 'issue', number, intent })
  const jobsFor = async (number: number) =>
    listOf(monitorImplementationJobSchema).parse(await json('/monitors/implementation-jobs?namespace=default&name=checkout-service')).items.filter((j) => j.issueNumber === number)

  it('settles the implementation job when the implement work finishes', async () => {
    const command = monitorCommandSchema.parse(await (await issueCommand(2221, 'implement')).json())
    expect((await jobsFor(2221)).find((j) => j.commandEventID === command.id)).toMatchObject({ phase: 'implementing', validationState: 'pending' })

    advance(60_000)
    expect((await jobsFor(2221)).find((j) => j.commandEventID === command.id)).toMatchObject({ phase: 'patch_ready', validationState: 'passed' })
  })

  it('cancels in-flight work on stop so it cannot finish over the stop', async () => {
    const implement = monitorCommandSchema.parse(await (await issueCommand(2219, 'implement')).json())
    expect((await issueCommand(2219, 'stop')).status).toBe(201)

    advance(60_000)
    const issue = listOf(monitorItemSchema).parse(await json('/monitors/repositories/checkout-service/items?namespace=default&kind=issue')).items.find((i) => i.number === 2219)
    expect(issue).toMatchObject({ workflowPhase: 'paused', skipReason: 'stopped_by_command' })
    const work = listOf(monitorWorkActionSchema).parse(await json('/monitors/work-actions?namespace=default&name=checkout-service')).items.find((w) => w.commandEventID === implement.id)
    expect(work).toMatchObject({ status: 'cancelled' })
    expect((await jobsFor(2219)).find((j) => j.commandEventID === implement.id)).toMatchObject({ phase: 'cancelled', error: 'stopped by command' })
  })

  it('rejects malformed commands', async () => {
    expect((await post('/monitors/repositories/checkout-service/commands?namespace=default', { kind: 'issue', number: 2214, intent: 'fix_ci' })).status).toBe(400)
    expect((await post('/monitors/repositories/checkout-service/commands?namespace=default', { kind: 'pull_request', number: 1482, intent: 'review', targetSHA: 'stale' })).status).toBe(409)
    expect((await post('/monitors/repositories/missing/commands?namespace=default', { kind: 'issue', number: 1, intent: 'triage' })).status).toBe(404)
  })

  it('creates a monitor that becomes ready after its first sync', async () => {
    const response = await post('/monitors/repositories', {
      name: 'payments-gateway',
      namespace: 'default',
      spec: { provider: 'github', repoURL: 'https://github.com/northwind-labs/payments-gateway', branch: 'main', agents: { reviewer: { name: 'code-reviewer' } } },
    })
    expect(response.status).toBe(201)
    expect(repositoryMonitorSchema.parse(await response.json()).status?.phase).toBe('Pending')
    advance(20_000)
    expect(repositoryMonitorSchema.parse(await json('/monitors/repositories/payments-gateway?namespace=default')).status?.phase).toBe('Ready')
  })
})
