import { http, HttpResponse, type RequestHandler } from 'msw'
import type { PatchProposal, RepositoryScan, ScanRun, SecurityFinding } from '@/schemas/security'
import {
  CLOSED_FINDING_STATES,
  droppedFindings,
  findings,
  patches,
  scanRuns,
  scans,
  slices,
  threatModels,
  viewScanRun,
  type ScanRunRecord,
} from '../data/security'
import { api, apiError, DEMO_NAMESPACE, digest, inDemoNamespace, list, noContent, notFound, now, query, uid } from '../shared'
import { monitorHandlers } from './monitors'

// Security scanning API: RepositoryScans, scan runs, review slices, threat
// models, findings, and patch proposals. Long-running work (scans, finding
// validation, patch generation) is modelled with completion timestamps and
// settled lazily on the next request, so the UI's polling sees it progress.

const SEVERITY_RANK: Record<string, number> = { critical: 4, high: 3, medium: 2, low: 1 }
const VALIDATION_RANK: Record<string, number> = { validated: 3, unvalidated: 2, pending: 1 }

/** Finding ID -> time its running validation finishes. */
const validationsDue = new Map<string, number>()
// Validations already running when the page loads finish a minute or so later.
for (const finding of findings) {
  if (finding.validationStatus === 'pending') validationsDue.set(finding.id, Date.now() + 75_000)
}

/** Next PR number per repository for PRs opened from patch proposals. */
const nextPRNumber: Record<string, number> = { 'checkout-service': 1490, 'inventory-api': 619, 'web-storefront': 879 }

function settle(nowMs = Date.now()) {
  for (const [id, dueMs] of validationsDue) {
    if (nowMs < dueMs) continue
    validationsDue.delete(id)
    const finding = findings.find((f) => f.id === id)
    if (!finding || finding.validationStatus !== 'pending') continue
    finding.validationStatus = 'validated'
    finding.updatedAt = new Date(dueMs).toISOString()
    finding.validationJSON = JSON.stringify({
      summary: `Reproduced "${finding.title}" against an isolated copy of ${finding.repositoryScan} at ${finding.commitSHA?.slice(0, 8) ?? 'HEAD'}.`,
      validation_steps: [
        `Built ${finding.repositoryScan} in a sandboxed workspace`,
        `Exercised ${finding.filePath ?? 'the affected code path'}${finding.line ? `:${finding.line}` : ''} with a crafted request`,
        'Captured the response and server logs as evidence',
      ],
      attack_path_analysis: 'An authenticated user can reach the vulnerable path from the public API without additional privileges.',
      assumptions: ['Default production configuration'],
      controls: ['Requests are authenticated'],
      blindspots: ['Downstream services were stubbed'],
    })
  }
  for (const record of patches) {
    const { proposal } = record
    if (proposal.status !== 'running' || !record.readyMs || nowMs < record.readyMs) continue
    proposal.status = 'succeeded'
    proposal.diffArtifact = 'patch.diff'
    proposal.summaryArtifact = 'patch-summary.md'
    proposal.updatedAt = new Date(record.readyMs).toISOString()
    const finding = findings.find((f) => f.id === proposal.findingID)
    if (finding && finding.state === 'patch_pending') {
      finding.state = 'patch_ready'
      finding.updatedAt = proposal.updatedAt
    }
  }
}

const runsFor = (repo: string) =>
  scanRuns.filter((record) => record.run.repositoryScan === repo).sort((a, b) => b.startedMs - a.startedMs)

/** A RepositoryScan with its status derived from runs, findings, and threat models. */
function viewScan(resource: RepositoryScan): RepositoryScan {
  const name = resource.metadata.name
  const problem = scans.find((record) => record.resource === resource)?.problem
  const runs = runsFor(name).map((record) => viewScanRun(record))
  const latest = runs[0]
  const lastSucceeded = runs.find((run) => run.phase === 'succeeded')
  const open = findings.filter((f) => f.repositoryScan === name && !CLOSED_FINDING_STATES.has(f.state))
  const count = (severity: string) => open.filter((f) => f.severity === severity).length
  const threatModel = threatModels.find((model) => model.repositoryScan === name)
  const phase = problem ? 'Error' : resource.spec.suspend ? 'Suspended' : latest?.phase === 'running' || latest?.phase === 'pending' ? 'Scanning' : 'Ready'
  return {
    ...resource,
    status: {
      ...resource.status,
      phase,
      lastScanID: latest?.id,
      lastScanTaskName: latest?.taskName,
      lastScanAt: latest?.startedAt,
      lastSuccessfulScanAt: lastSucceeded?.completedAt,
      threatModelVersion: threatModel?.version ?? 0,
      findingCounts: { total: open.length, critical: count('critical'), high: count('high'), medium: count('medium'), low: count('low') },
      conditions: problem
        ? [{ type: 'Ready', status: 'False', reason: problem.reason, message: problem.message, lastTransitionTime: resource.metadata.creationTimestamp }]
        : [{ type: 'Ready', status: 'True', reason: phase === 'Scanning' ? 'ScanRunning' : 'ScanSucceeded', message: latest?.summary ?? 'Waiting for the first scan', lastTransitionTime: latest?.startedAt ?? resource.metadata.creationTimestamp }],
    },
  }
}

const findScan = (name: string) => scans.find((record) => record.resource.metadata.name === name)

function startScan(repo: string, mode: 'incremental' | 'manual', durationMs: number): ScanRunRecord {
  const existing = runsFor(repo)
  const seq = existing.reduce((max, record) => Math.max(max, Number(record.run.id.split('_').pop()) || 0), 0) + 1
  const prefix = repo.split('-')[0]
  const startedMs = Date.now()
  const sliceCount = slices.filter((s) => s.repositoryScan === repo).length || 6
  const previousHead = existing[0]?.run.headCommit ?? digest(`${repo}:base`)
  const record: ScanRunRecord = {
    run: {
      id: `scan_${prefix}_${String(seq).padStart(3, '0')}`,
      namespace: DEMO_NAMESPACE,
      repositoryScan: repo,
      taskName: `security-scan-${prefix}-${digest(`${repo}:${seq}:${startedMs}`, 5)}`,
      mode,
      phase: 'running',
      startedAt: new Date(startedMs).toISOString(),
      baseCommit: previousHead,
      headCommit: previousHead,
      commitCount: 0,
      sliceCount,
      reviewedSliceCount: sliceCount,
      skippedSliceCount: 0,
      acceptedFindings: 0,
      droppedFindings: 0,
      scannerPolicyVersion: 'v2.3',
      policyDigest: `sha256:${digest(`policy:${repo}`, 16)}`,
      idempotencyKey: `${repo}:${digest(`${repo}:${seq}:${startedMs}`, 12)}`,
    },
    startedMs,
    finishMs: startedMs + durationMs,
    outcome: 'succeeded',
    finalSummary: `Re-reviewed ${sliceCount} slices at the current head. No new findings; existing findings were re-confirmed.`,
  }
  scanRuns.push(record)
  return record
}

async function readJSON<T>(request: Request): Promise<T | undefined> {
  try {
    return (await request.json()) as T
  } catch {
    return undefined
  }
}

function filterFindings(repo: string, request: Request): SecurityFinding[] {
  let result = findings.filter((f) => f.repositoryScan === repo)
  const filters: Array<[string, (f: SecurityFinding, value: string) => boolean]> = [
    ['sliceID', (f, v) => f.sliceID === v],
    ['category', (f, v) => f.category === v],
    ['severity', (f, v) => f.severity === v],
    ['validationStatus', (f, v) => f.validationStatus === v],
    ['state', (f, v) => f.state === v],
  ]
  for (const [key, matches] of filters) {
    const value = query(request, key)
    if (value) result = result.filter((f) => matches(f, value))
  }
  if (query(request, 'recommended') === 'true') {
    result = result
      .filter((f) => f.validationStatus !== 'failed' && !CLOSED_FINDING_STATES.has(f.state))
      .sort((a, b) =>
        (SEVERITY_RANK[b.severity] ?? 0) - (SEVERITY_RANK[a.severity] ?? 0) ||
        (VALIDATION_RANK[b.validationStatus] ?? 0) - (VALIDATION_RANK[a.validationStatus] ?? 0))
  } else {
    result = [...result].sort((a, b) => b.createdAt.localeCompare(a.createdAt))
  }
  return result
}

const proposalsFor = (findingID: string): PatchProposal[] =>
  patches.filter((record) => record.proposal.findingID === findingID).map((record) => record.proposal).sort((a, b) => b.createdAt.localeCompare(a.createdAt))

const findFinding = (id: string) => findings.find((f) => f.id === id)

const securityRoutes: RequestHandler[] = [
  http.get(api('/security/repositories'), ({ request }) => {
    settle()
    return list(inDemoNamespace(request) ? scans.map((record) => viewScan(record.resource)) : [])
  }),

  http.post(api('/security/repositories'), async ({ request }) => {
    const body = await readJSON<{ name?: string; namespace?: string; spec?: RepositoryScan['spec'] }>(request)
    const name = body?.name?.trim()
    if (!body || !name || !body.spec?.repoURL || !body.spec.analysisAgentRef?.name) {
      return apiError(400, 'name, spec.repoURL, and spec.analysisAgentRef are required')
    }
    if ((body.namespace ?? DEMO_NAMESPACE) !== DEMO_NAMESPACE) return apiError(403, `the demo only accepts repositories in namespace ${DEMO_NAMESPACE}`)
    if (findScan(name)) return apiError(409, `repositoryscans.orka.ai "${name}" already exists`)
    const resource: RepositoryScan = {
      apiVersion: 'core.orka.ai/v1alpha1',
      kind: 'RepositoryScan',
      metadata: { name, namespace: DEMO_NAMESPACE, uid: uid(`repositoryscan:${name}:${Date.now()}`), creationTimestamp: now() },
      spec: { provider: 'github', branch: 'main', ...body.spec },
      status: { threatModelVersion: 0 },
    }
    scans.push({ resource })
    // The controller starts the first scan as soon as the repository is admitted.
    startScan(name, 'incremental', 45_000)
    return HttpResponse.json(viewScan(resource), { status: 201 })
  }),

  http.get(api('/security/repositories/:name'), ({ request, params }) => {
    settle()
    const record = inDemoNamespace(request) ? findScan(String(params.name)) : undefined
    return record ? HttpResponse.json(viewScan(record.resource)) : notFound(`repository scan ${params.name}`)
  }),

  http.get(api('/security/repositories/:name/threat-model'), ({ request, params }) => {
    const model = inDemoNamespace(request) ? threatModels.find((m) => m.repositoryScan === params.name) : undefined
    return model ? HttpResponse.json(model) : notFound('threat model')
  }),

  http.put(api('/security/repositories/:name/threat-model'), async ({ request, params }) => {
    const name = String(params.name)
    if (!inDemoNamespace(request) || !findScan(name)) return notFound(`repository scan ${name}`)
    const body = await readJSON<{ content?: string; source?: string; expectedVersion?: number }>(request)
    if (!body || typeof body.content !== 'string') return apiError(400, 'content is required')
    const index = threatModels.findIndex((m) => m.repositoryScan === name)
    const current = index >= 0 ? threatModels[index] : undefined
    const currentVersion = current?.version ?? 0
    if (body.expectedVersion !== undefined && body.expectedVersion !== currentVersion) {
      return apiError(409, `threat model version is ${currentVersion}, expected ${body.expectedVersion}`)
    }
    const timestamp = now()
    const saved = {
      namespace: DEMO_NAMESPACE,
      repositoryScan: name,
      version: currentVersion + 1,
      content: body.content,
      source: body.source || 'edited',
      generatedByScan: current?.generatedByScan,
      createdAt: current?.createdAt ?? timestamp,
      updatedAt: timestamp,
    }
    if (index >= 0) threatModels[index] = saved
    else threatModels.push(saved)
    return HttpResponse.json(saved)
  }),

  http.get(api('/security/repositories/:name/scans'), ({ request, params }) => {
    if (!inDemoNamespace(request)) return list([])
    return list(runsFor(String(params.name)).map((record) => viewScanRun(record)))
  }),

  http.post(api('/security/repositories/:name/scans'), ({ request, params }) => {
    const name = String(params.name)
    const record = inDemoNamespace(request) ? findScan(name) : undefined
    if (!record) return notFound(`repository scan ${name}`)
    if (record.problem) return apiError(409, `repository scan is not ready: ${record.problem.reason}`)
    if (runsFor(name).some((run) => viewScanRun(run).phase === 'running')) return apiError(409, 'a scan is already running for this repository')
    const run: ScanRun = viewScanRun(startScan(name, 'manual', 40_000))
    return HttpResponse.json(run, { status: 201 })
  }),

  http.get(api('/security/repositories/:name/slices'), ({ request, params }) =>
    list(inDemoNamespace(request) ? slices.filter((s) => s.repositoryScan === params.name) : [])),

  http.get(api('/security/repositories/:name/dropped-findings'), ({ request, params }) => {
    if (!inDemoNamespace(request)) return list([])
    const scanRunID = query(request, 'scanRunID')
    return list(droppedFindings.filter((d) => d.repositoryScan === params.name && (!scanRunID || d.scanRunID === scanRunID)))
  }),

  http.get(api('/security/repositories/:name/findings'), ({ request, params }) => {
    settle()
    if (!inDemoNamespace(request)) return list([])
    const all = filterFindings(String(params.name), request)
    const limit = Math.max(1, Number(query(request, 'limit') ?? '50') || 50)
    const offset = Math.max(0, Number(query(request, 'cursor') ?? '0') || 0)
    const page = all.slice(offset, offset + limit)
    const next = offset + limit < all.length ? String(offset + limit) : undefined
    return HttpResponse.json({ items: page, metadata: next ? { continue: next } : {} })
  }),

  http.get(api('/security/findings/:id'), ({ request, params }) => {
    settle()
    const finding = inDemoNamespace(request) ? findFinding(String(params.id)) : undefined
    return finding ? HttpResponse.json(finding) : notFound('finding')
  }),

  http.post(api('/security/findings/:id/dismiss'), ({ request, params }) => {
    const finding = inDemoNamespace(request) ? findFinding(String(params.id)) : undefined
    if (!finding) return notFound('finding')
    finding.state = 'dismissed'
    finding.updatedAt = now()
    return noContent()
  }),

  http.post(api('/security/findings/:id/reopen'), ({ request, params }) => {
    const finding = inDemoNamespace(request) ? findFinding(String(params.id)) : undefined
    if (!finding) return notFound('finding')
    finding.state = 'open'
    finding.updatedAt = now()
    return noContent()
  }),

  http.post(api('/security/findings/:id/validate'), ({ request, params }) => {
    const finding = inDemoNamespace(request) ? findFinding(String(params.id)) : undefined
    if (!finding) return notFound('finding')
    if (finding.validationStatus === 'pending') return apiError(409, 'validation is already running')
    finding.validationStatus = 'pending'
    finding.updatedAt = now()
    validationsDue.set(finding.id, Date.now() + 25_000)
    const taskName = `security-validate-${finding.repositoryScan.split('-')[0]}-${digest(`${finding.id}:${Date.now()}`, 5)}`
    return HttpResponse.json({ status: 'pending', taskName }, { status: 202 })
  }),

  http.post(api('/security/findings/:id/patch'), ({ request, params }) => {
    settle()
    const finding = inDemoNamespace(request) ? findFinding(String(params.id)) : undefined
    if (!finding) return notFound('finding')
    if (patches.some((record) => record.proposal.findingID === finding.id && record.proposal.status === 'running')) {
      return apiError(409, 'patch generation is already running for this finding')
    }
    const timestamp = now()
    const slug = finding.title.toLowerCase().replace(/[^a-z0-9]+/g, '-').split('-').filter(Boolean).slice(0, 4).join('-')
    const proposal: PatchProposal = {
      id: `patch_${digest(`${finding.id}:${Date.now()}`, 10)}`,
      namespace: DEMO_NAMESPACE,
      repositoryScan: finding.repositoryScan,
      findingID: finding.id,
      taskName: `security-patch-${slug}-${digest(`${finding.id}:${Date.now()}:task`, 5)}`,
      branch: `orka/security/${slug}`,
      status: 'running',
      createdAt: timestamp,
      updatedAt: timestamp,
    }
    patches.push({ proposal, readyMs: Date.now() + 20_000 })
    if (!['pr_open', 'fixed', 'resolved'].includes(finding.state)) finding.state = 'patch_pending'
    finding.patchProposalID = proposal.id
    finding.updatedAt = timestamp
    return HttpResponse.json(proposal, { status: 201 })
  }),

  http.get(api('/security/findings/:id/patches'), ({ request, params }) => {
    settle()
    return list(inDemoNamespace(request) ? proposalsFor(String(params.id)) : [])
  }),

  http.post(api('/security/findings/:id/pull-request'), ({ request, params }) => {
    settle()
    const finding = inDemoNamespace(request) ? findFinding(String(params.id)) : undefined
    if (!finding) return notFound('finding')
    const proposal = proposalsFor(finding.id).find((p) => p.status === 'succeeded' || p.status === 'pr_opened')
    if (!proposal) return apiError(409, 'no successful patch proposal to open a pull request from')
    if (proposal.status !== 'pr_opened') {
      const repoURL = findScan(finding.repositoryScan)?.resource.spec.repoURL ?? ''
      const number = nextPRNumber[finding.repositoryScan] ?? 100
      nextPRNumber[finding.repositoryScan] = number + 1
      proposal.status = 'pr_opened'
      proposal.prNumber = number
      proposal.prURL = `${repoURL}/pull/${number}`
      proposal.updatedAt = now()
    }
    finding.state = 'pr_open'
    finding.prNumber = proposal.prNumber
    finding.prURL = proposal.prURL
    finding.patchProposalID = proposal.id
    finding.updatedAt = now()
    return HttpResponse.json({ prURL: proposal.prURL, prNumber: proposal.prNumber, status: proposal.status }, { status: 201 })
  }),
]

/** Security scans, findings, patches, threat models, and repository monitors. */
export const securityHandlers: RequestHandler[] = [...securityRoutes, ...monitorHandlers]
