import type {
  MonitorAction,
  MonitorCommand,
  MonitorImplementationJob,
  MonitorItem,
  MonitorMutation,
  MonitorRun,
  MonitorWorkAction,
  RepositoryMonitor,
} from '@/schemas/monitor'
import { ago, DEMO_NAMESPACE, digest, uid } from '../shared'
import { AGENTS, ORG, REPOS, type RepoKey } from '../world'

// Repository monitor fixtures: maintainer automation on the Northwind Labs
// repositories. PR queues, issue inventories, and audit trails reference the
// review/triage/fix tasks in world.ts. Handlers mutate these arrays in place.

const ns = DEMO_NAMESPACE
const MINUTE = 60_000
const BOT = 'northwind-orka[bot]'

const sha = (seed: string) => digest(`monitor:${seed}`)

function monitor(name: string, repo: RepoKey, createdAgo: number, spec: Partial<RepositoryMonitor['spec']>, status: NonNullable<RepositoryMonitor['status']>): RepositoryMonitor {
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'RepositoryMonitor',
    metadata: { name, namespace: ns, uid: uid(`repositorymonitor:${name}`), creationTimestamp: ago(createdAgo) },
    spec: {
      provider: 'github',
      repoURL: REPOS[repo].url,
      owner: ORG,
      repository: REPOS[repo].slug.split('/')[1],
      branch: 'main',
      readCredentialRef: { name: 'github-read-token' },
      forgeCredentialRef: { name: 'github-app-checkout' },
      ...spec,
    },
    status: { observedGeneration: 1, ...status },
  }
}

const reviewerAgents = {
  reviewer: { name: AGENTS.reviewer.name },
  triager: { name: AGENTS.triage.name },
  implementer: { name: AGENTS.fixer.name },
  repairer: { name: AGENTS.fixer.name },
  planner: { name: AGENTS.coordinator.name },
}

const inlinePublish = {
  enabled: true, mode: 'inline', event: 'COMMENT', postPassed: true, postNeedsChanges: true, postNeedsHuman: true, postSecuritySensitive: false,
  inline: { enabled: true, minPriority: 'P2', maxComments: 10 },
}

const readyCondition = (minutesAgo: number) => [
  { type: 'Ready', status: 'True', reason: 'Reconciled', message: 'Monitor is polling GitHub', lastTransitionTime: ago(minutesAgo) },
]

export const monitors: RepositoryMonitor[] = [
  monitor('checkout-service', 'checkout', 60 * 24 * 34, {
    schedule: '*/15 * * * *', timeZone: 'America/Los_Angeles',
    targets: {
      pullRequests: { enabled: true, includeDrafts: false, maxPerRun: 10 },
      issues: { enabled: true, maxPerRun: 5, includeLabels: ['bug', 'enhancement'], excludeLabels: ['wontfix'] },
    },
    agents: reviewerAgents,
    triggers: { github: { labels: { enabled: true, consumeCommandLabels: true, requireActorPermission: 'write', issues: { implement: 'orka:implement' } } } },
    issueWorkflow: {
      triage: { enabled: true }, research: { enabled: true }, planning: { enabled: true },
      implementation: { enabled: true, requirePlan: true, branchPrefix: 'orka/issue-', maxActive: 2, maxAttemptsPerIssue: 3, maxChangedFiles: 20, allowedPaths: ['internal/**', 'cmd/**'] },
    },
    review: { event: 'COMMENT', requireGreenCI: true, staleReviewTTL: '72h', exactEventEnabled: true, publish: inlinePublish },
    repair: { enabled: true, maxRepairsPerPR: 3, maxRepairsPerHead: 1, maxValidationRetries: 2 },
    policy: { protectedLabels: ['do-not-merge', 'security'], pauseLabels: ['orka:pause'], allowedRepositoryPermissions: ['write', 'maintain', 'admin'] },
    validation: { image: 'ghcr.io/northwind-labs/checkout-ci:go1.25', mode: 'container', commands: ['make lint', 'go test ./...'] },
  }, {
    phase: 'Ready', conditions: readyCondition(60 * 24 * 2),
    openPullRequests: 6, pendingReviews: 2, activeRepairs: 1, blockedItems: 0, mergeReadyItems: 2,
    openIssues: 17, pendingIssueActions: 2, blockedIssues: 1,
  }),
  monitor('inventory-api', 'inventory', 60 * 24 * 21, {
    schedule: '*/30 * * * *', timeZone: 'Europe/Berlin',
    targets: {
      pullRequests: { enabled: true, includeDrafts: false, maxPerRun: 5 },
      issues: { enabled: true, maxPerRun: 10 },
    },
    agents: { reviewer: { name: AGENTS.reviewer.name }, triager: { name: AGENTS.triage.name }, implementer: { name: AGENTS.fixer.name } },
    issueWorkflow: { triage: { enabled: true }, implementation: { enabled: true, requirePlan: false, branchPrefix: 'orka/issue-', maxActive: 1, maxAttemptsPerIssue: 2, maxChangedFiles: 20 } },
    review: { event: 'COMMENT', requireGreenCI: false, staleReviewTTL: '48h', publish: { enabled: true, mode: 'summary_only', event: 'COMMENT', postPassed: false, postNeedsChanges: true, postNeedsHuman: true } },
    validation: { image: 'python:3.13-slim', mode: 'container', commands: ['pip install -e .[test]', 'pytest -q'] },
  }, {
    phase: 'Ready', conditions: readyCondition(60 * 24 * 5),
    openPullRequests: 2, pendingReviews: 0, activeRepairs: 0, blockedItems: 0, mergeReadyItems: 1,
    openIssues: 9, pendingIssueActions: 1, blockedIssues: 1,
  }),
  monitor('web-storefront', 'storefront', 60 * 24 * 9, {
    schedule: '*/20 * * * *',
    targets: { pullRequests: { enabled: true, includeDrafts: true, maxPerRun: 10 } },
    agents: { reviewer: { name: AGENTS.reviewer.name } },
    review: { event: 'COMMENT', requireGreenCI: true, staleReviewTTL: '24h', publish: inlinePublish },
    policy: { pauseLabels: ['orka:pause'] },
  }, {
    phase: 'Ready', conditions: readyCondition(60 * 24),
    openPullRequests: 3, pendingReviews: 1, activeRepairs: 0, blockedItems: 0, mergeReadyItems: 1,
    openIssues: 0, pendingIssueActions: 0, blockedIssues: 0,
  }),
  monitor('platform-infra', 'infra', 60 * 24 * 4, {
    schedule: '0 * * * *', suspend: true,
    targets: { pullRequests: { enabled: true, maxPerRun: 5 } },
    agents: { reviewer: { name: AGENTS.reviewer.name } },
    review: { event: 'COMMENT', publish: { enabled: false } },
  }, {
    phase: 'Suspended',
    conditions: [{ type: 'Ready', status: 'False', reason: 'Suspended', message: 'spec.suspend is true', lastTransitionTime: ago(60 * 24 * 2) }],
    openPullRequests: 4, pendingReviews: 0, activeRepairs: 0, blockedItems: 0, mergeReadyItems: 0,
  }),
]

// ---- Runs ----

export interface MonitorRunRecord {
  run: MonitorRun
  startedMs: number
  finishMs: number
  result: Pick<MonitorRun, 'selectedCount' | 'createdTaskCount' | 'skippedCount' | 'error'> & { phase: 'succeeded' | 'failed' }
}

function run(monitorName: string, n: number, trigger: string, startedAgoMin: number, durationSec: number, result: MonitorRunRecord['result'], target?: Pick<MonitorRun, 'targetKind' | 'targetNumber' | 'targetSHA' | 'commandEventID'>): MonitorRunRecord {
  const startedMs = Date.now() - startedAgoMin * MINUTE
  return {
    run: {
      id: `run_${monitorName.split('-')[0]}_${String(n).padStart(4, '0')}`,
      monitorNamespace: ns,
      monitorName,
      trigger,
      phase: 'running',
      startedAt: new Date(startedMs).toISOString(),
      ...target,
    },
    startedMs,
    finishMs: startedMs + durationSec * 1000,
    result,
  }
}

export const runs: MonitorRunRecord[] = [
  run('checkout-service', 2291, 'schedule', 2, 25, { phase: 'succeeded', selectedCount: 3, createdTaskCount: 1, skippedCount: 2 }),
  run('checkout-service', 2290, 'command', 12, 18, { phase: 'succeeded', selectedCount: 1, createdTaskCount: 1, skippedCount: 0 }, { targetKind: 'issue', targetNumber: 2214, commandEventID: 'cmd_checkout_0183' }),
  run('checkout-service', 2289, 'schedule', 17, 22, { phase: 'succeeded', selectedCount: 4, createdTaskCount: 0, skippedCount: 4 }),
  run('checkout-service', 2288, 'webhook', 53, 9, { phase: 'succeeded', selectedCount: 1, createdTaskCount: 1, skippedCount: 0 }, { targetKind: 'pull_request', targetNumber: 1479, targetSHA: sha('pr1479') }),
  run('checkout-service', 2287, 'schedule', 32, 40, { phase: 'failed', selectedCount: 0, createdTaskCount: 0, skippedCount: 0, error: 'GitHub API rate limit exceeded (secondary limit); retry scheduled' }),
  run('inventory-api', 811, 'schedule', 8, 14, { phase: 'succeeded', selectedCount: 2, createdTaskCount: 0, skippedCount: 2 }),
  run('inventory-api', 810, 'command', 39, 12, { phase: 'succeeded', selectedCount: 1, createdTaskCount: 1, skippedCount: 0 }, { targetKind: 'issue', targetNumber: 3391, commandEventID: 'cmd_inventory_0042' }),
  run('inventory-api', 809, 'schedule', 38, 16, { phase: 'succeeded', selectedCount: 3, createdTaskCount: 1, skippedCount: 2 }),
  run('web-storefront', 377, 'schedule', 6, 11, { phase: 'succeeded', selectedCount: 2, createdTaskCount: 0, skippedCount: 2 }),
  run('web-storefront', 376, 'webhook', 96, 8, { phase: 'succeeded', selectedCount: 1, createdTaskCount: 1, skippedCount: 0 }, { targetKind: 'pull_request', targetNumber: 873, targetSHA: sha('pr873') }),
  run('platform-infra', 52, 'schedule', 60 * 24 * 2 + 10, 7, { phase: 'succeeded', selectedCount: 4, createdTaskCount: 2, skippedCount: 2 }),
]

export function viewMonitorRun(record: MonitorRunRecord, nowMs = Date.now()): MonitorRun {
  if (nowMs < record.finishMs) return record.run
  return { ...record.run, ...record.result, completedAt: new Date(record.finishMs).toISOString() }
}

// ---- Items (PR queue and issue inventory) ----

type ItemSeed = Omit<MonitorItem, 'monitorNamespace' | 'itemKey' | 'updatedAt' | 'lastSeenAt'> & { monitorName: string; updatedAgo: number; labels?: string[] }

function item(seed: ItemSeed): MonitorItem {
  const { updatedAgo, labels, monitorName, ...rest } = seed
  const repo = monitors.find((m) => m.metadata.name === monitorName)?.spec.repoURL ?? ''
  const path = seed.kind === 'pull_request' ? 'pull' : 'issues'
  return {
    monitorNamespace: ns,
    monitorName,
    itemKey: `${seed.kind}/${seed.number}`,
    htmlURL: `${repo}/${path}/${seed.number}`,
    labelsJSON: JSON.stringify(labels ?? []),
    snapshotDigest: `sha256:${digest(`snapshot:${monitorName}:${seed.number}`, 16)}`,
    githubUpdatedAt: ago(updatedAgo),
    updatedAt: ago(updatedAgo),
    lastSeenAt: ago(Math.min(updatedAgo, 2)),
    ...rest,
  }
}

const reviewURL = (monitorName: string, number: number, id: number) =>
  `${monitors.find((m) => m.metadata.name === monitorName)?.spec.repoURL}/pull/${number}#pullrequestreview-${id}`

export const items: MonitorItem[] = [
  // checkout-service pull requests
  item({
    monitorName: 'checkout-service', kind: 'pull_request', number: 1486, title: 'fix(refunds): scope refund lookup to the calling merchant', author: BOT, state: 'open',
    baseBranch: 'main', headBranch: 'orka/security/refund-merchant-scope', headSHA: sha('pr1486'), ciState: 'pending', lastVerdict: 'queued', labels: ['security', 'orka'], updatedAgo: 95,
  }),
  item({
    monitorName: 'checkout-service', kind: 'pull_request', number: 1482, title: 'Add idempotency keys to payment capture', author: 'dev.patel', state: 'open',
    baseBranch: 'main', headBranch: 'dev/capture-idempotency', headSHA: sha('pr1482'), ciState: 'passed', lastVerdict: 'queued',
    lastActionKind: 'review', lastActionTaskName: 'review-pr-1482-q8m4d', labels: ['payments'], updatedAgo: 1,
  }),
  item({
    monitorName: 'checkout-service', kind: 'pull_request', number: 1479, title: 'Migrate order events to the outbox pattern', author: 'maya.chen', state: 'open',
    baseBranch: 'main', headBranch: 'maya/outbox-relay', headSHA: sha('pr1479'), ciState: 'passed', lastVerdict: 'passed', automergeState: 'merge_ready',
    lastActionKind: 'review', lastActionTaskName: 'review-pr-1479-b7n2k',
    lastPublishID: 'pub_checkout_0391', lastPublishPhase: 'succeeded', lastPublishURL: reviewURL('checkout-service', 1479, 2841177390), labels: ['architecture'], updatedAgo: 46,
  }),
  item({
    monitorName: 'checkout-service', kind: 'pull_request', number: 1477, title: 'build(deps): bump golang.org/x/net from 0.35.0 to 0.38.0', author: 'dependabot[bot]', state: 'open',
    baseBranch: 'main', headBranch: 'dependabot/go_modules/golang.org/x/net-0.38.0', headSHA: sha('pr1477'), ciState: 'passed', lastVerdict: 'passed', automergeState: 'merge_ready',
    lastPublishID: 'pub_checkout_0388', lastPublishPhase: 'succeeded', lastPublishURL: reviewURL('checkout-service', 1477, 2840923311), labels: ['dependencies'], updatedAgo: 180,
  }),
  item({
    monitorName: 'checkout-service', kind: 'pull_request', number: 1474, title: 'Retry Stripe webhooks with jittered backoff', author: 'sam.okafor', state: 'open',
    baseBranch: 'main', headBranch: 'sam/webhook-backoff', headSHA: sha('pr1474'), ciState: 'failed', lastVerdict: 'needs_changes', repairState: 'running',
    lastCommandIntent: 'fix_ci', lastCommandID: 'cmd_checkout_0181',
    lastPublishID: 'pub_checkout_0385', lastPublishPhase: 'succeeded', lastPublishURL: reviewURL('checkout-service', 1474, 2840377102), labels: ['payments', 'reliability'], updatedAgo: 22,
  }),
  item({
    monitorName: 'checkout-service', kind: 'pull_request', number: 1471, title: 'Remove the legacy v1 checkout endpoint', author: 'jordan.reyes', state: 'open',
    baseBranch: 'main', headBranch: 'jordan/remove-v1-checkout', headSHA: sha('pr1471'), ciState: 'passed', lastVerdict: 'security_sensitive',
    lastPublishPhase: 'skipped', lastPublishReason: 'postSecuritySensitive is false', labels: ['breaking-change'], updatedAgo: 300,
  }),
  // checkout-service issues
  item({
    monitorName: 'checkout-service', kind: 'issue', number: 2221, title: 'Refund confirmation emails sent twice when the processor retries', author: 'lena.fischer', state: 'open',
    workflowPhase: 'triage_queued', labels: ['bug'], updatedAgo: 3,
  }),
  item({
    monitorName: 'checkout-service', kind: 'issue', number: 2219, title: 'Support partial captures for split shipments', author: 'maya.chen', state: 'open',
    workflowPhase: 'plan_ready', lastActionKind: 'plan', lastActionID: 'act_checkout_0712', labels: ['enhancement', 'payments'], updatedAgo: 140,
  }),
  item({
    monitorName: 'checkout-service', kind: 'issue', number: 2216, title: 'Apple Pay button missing on Safari 18', author: 'customer-success', state: 'open',
    workflowPhase: 'triaged', lastActionKind: 'triage', lastActionTaskName: 'triage-issue-2216-w9d3e', labels: ['bug', 'frontend', 'p2'], updatedAgo: 205,
  }),
  item({
    monitorName: 'checkout-service', kind: 'issue', number: 2214, title: 'Cart total off by one cent with percentage discounts on JPY items', author: 'sam.okafor', state: 'open',
    workflowPhase: 'implementing', lastCommandIntent: 'implement', lastCommandID: 'cmd_checkout_0183',
    lastActionKind: 'implement', lastActionTaskName: 'fix-cart-rounding-bug-x9m4d', labels: ['bug', 'payments', 'orka:implement'], updatedAgo: 11,
  }),
  item({
    monitorName: 'checkout-service', kind: 'issue', number: 2205, title: 'Document webhook signing secret rotation', author: 'dev.patel', state: 'open',
    workflowPhase: 'paused', skipReason: 'pause label orka:pause present', labels: ['docs', 'orka:pause'], updatedAgo: 60 * 26,
  }),
  // inventory-api
  item({
    monitorName: 'inventory-api', kind: 'pull_request', number: 615, title: 'Lock the SKU row in reserve() with SELECT ... FOR UPDATE', author: 'dev.patel', state: 'open',
    baseBranch: 'main', headBranch: 'dev/reserve-row-lock', headSHA: sha('pr615'), ciState: 'passed', lastVerdict: 'needs_changes',
    lastPublishID: 'pub_inventory_0102', lastPublishPhase: 'succeeded', lastPublishURL: reviewURL('inventory-api', 615, 2839981245), labels: ['bug'], updatedAgo: 27,
  }),
  item({
    monitorName: 'inventory-api', kind: 'pull_request', number: 612, title: 'chore(deps): weekly Python dependency bump (2026-10-08)', author: BOT, state: 'open',
    baseBranch: 'main', headBranch: 'orka/deps-weekly-20261008', headSHA: sha('pr612'), ciState: 'passed', lastVerdict: 'passed', automergeState: 'merge_ready',
    lastPublishPhase: 'skipped', lastPublishReason: 'postPassed is false', labels: ['dependencies'], updatedAgo: 60 * 24 * 6,
  }),
  item({
    monitorName: 'inventory-api', kind: 'issue', number: 3394, title: 'Reservation expiry job logs at DEBUG in production', author: 'jordan.reyes', state: 'open',
    workflowPhase: 'triage_queued', labels: [], updatedAgo: 6,
  }),
  item({
    monitorName: 'inventory-api', kind: 'issue', number: 3391, title: 'Stock count goes negative after concurrent reservations', author: 'warehouse-ops', state: 'open',
    workflowPhase: 'triaged', lastActionKind: 'triage', lastActionTaskName: 'triage-issue-3391-m2x7c', linkedPRNumber: 615, labels: ['bug', 'p1', 'concurrency'], updatedAgo: 36,
  }),
  item({
    monitorName: 'inventory-api', kind: 'issue', number: 3388, title: 'Add bulk SKU import endpoint', author: 'lena.fischer', state: 'open',
    workflowPhase: 'triaged', lastActionKind: 'triage', lastActionTaskName: 'triage-issue-3388-k4v1s', labels: ['enhancement'], updatedAgo: 138,
  }),
  item({
    monitorName: 'inventory-api', kind: 'issue', number: 3380, title: 'Migrate warehouse adapters to the v3 scanner SDK', author: 'sam.okafor', state: 'open',
    workflowPhase: 'implementation_queued', skipReason: 'implementation_active_budget_exhausted', labels: ['enhancement', 'orka:implement'], updatedAgo: 60 * 20,
  }),
  // web-storefront
  item({
    monitorName: 'web-storefront', kind: 'pull_request', number: 876, title: 'Release v2.14.0', author: 'maya.chen', state: 'open',
    baseBranch: 'main', headBranch: 'release/v2.14.0', headSHA: sha('pr876'), ciState: 'pending', lastVerdict: 'unseen', labels: ['release'], updatedAgo: 18,
  }),
  item({
    monitorName: 'web-storefront', kind: 'pull_request', number: 873, title: 'Lazy-load product carousel images', author: 'jordan.reyes', state: 'open',
    baseBranch: 'main', headBranch: 'jordan/carousel-lazy', headSHA: sha('pr873'), ciState: 'passed', lastVerdict: 'passed', automergeState: 'merge_ready',
    lastActionKind: 'review', lastActionTaskName: 'review-pr-873-f5j9w',
    lastPublishID: 'pub_storefront_0057', lastPublishPhase: 'succeeded', lastPublishURL: reviewURL('web-storefront', 873, 2838110984), labels: ['performance'], updatedAgo: 90,
  }),
  item({
    monitorName: 'web-storefront', kind: 'pull_request', number: 869, title: 'WIP: new PDP layout behind feature flag', author: 'lena.fischer', state: 'open',
    baseBranch: 'main', headBranch: 'lena/pdp-layout', headSHA: sha('pr869'), ciState: 'failed', lastVerdict: 'skipped', skipReason: 'draft pull request', labels: ['wip'], updatedAgo: 60 * 30,
  }),
]

// ---- Actions (agent outputs recorded per item) ----

function action(monitorName: string, id: string, fields: Omit<MonitorAction, 'id' | 'monitorNamespace' | 'monitorName' | 'createdAt'>, createdAgo: number): MonitorAction {
  return { id, monitorNamespace: ns, monitorName, monitorGeneration: 1, createdAt: ago(createdAgo), ...fields }
}

export const actions: MonitorAction[] = [
  action('checkout-service', 'act_checkout_0718', { kind: 'issue', number: 2214, actionKind: 'implement', taskName: 'fix-cart-rounding-bug-x9m4d', commandEventID: 'cmd_checkout_0183', workActionID: 'wa_checkout_0441', summary: 'Implementation started on branch orka/issue-2214-jpy-rounding.' }, 11),
  action('checkout-service', 'act_checkout_0716', { kind: 'pull_request', number: 1474, actionKind: 'repair', headSHA: sha('pr1474'), verdict: 'running', summary: 'Repairing failing TestWebhookRetry_Backoff (expected jitter bounds off by one).' }, 21),
  action('checkout-service', 'act_checkout_0714', { kind: 'pull_request', number: 1479, actionKind: 'review', headSHA: sha('pr1479'), taskName: 'review-pr-1479-b7n2k', verdict: 'passed', confidence: 'high', summary: 'Outbox write happens in the same transaction as the order update; relay is idempotent on event ID. Two minor nits posted inline.' }, 46),
  action('checkout-service', 'act_checkout_0712', { kind: 'issue', number: 2219, actionKind: 'plan', verdict: 'ready', confidence: 'medium', summary: 'Plan: add capture_line_items table, split CaptureRequest by shipment, keep single-capture path as the default.' }, 140),
  action('checkout-service', 'act_checkout_0709', { kind: 'issue', number: 2216, actionKind: 'triage', taskName: 'triage-issue-2216-w9d3e', verdict: 'success', confidence: 'high', summary: 'Labelled bug, frontend, p2. Likely caused by the Apple Pay JS SDK feature check removed in #1466.' }, 205),
  action('checkout-service', 'act_checkout_0705', { kind: 'pull_request', number: 1471, actionKind: 'review', headSHA: sha('pr1471'), verdict: 'security_sensitive', confidence: 'medium', summary: 'Removes an authenticated endpoint still referenced by the mobile app v5.2; needs a human decision on the deprecation window.' }, 300),
  action('inventory-api', 'act_inventory_0233', { kind: 'pull_request', number: 615, actionKind: 'review', headSHA: sha('pr615'), verdict: 'needs_changes', confidence: 'high', summary: 'Row lock is correct, but release() still decrements without the lock; see inline comment on service.py:112.' }, 27),
  action('inventory-api', 'act_inventory_0231', { kind: 'issue', number: 3391, actionKind: 'triage', taskName: 'triage-issue-3391-m2x7c', verdict: 'success', confidence: 'high', summary: 'Confirmed race in reserve(); linked to security finding "Concurrent reservations can drive stock negative". Labelled p1.' }, 37),
  action('inventory-api', 'act_inventory_0228', { kind: 'issue', number: 3388, actionKind: 'triage', taskName: 'triage-issue-3388-k4v1s', verdict: 'success', confidence: 'medium', summary: 'Feature request; suggested reusing the CSV import pipeline from exports.' }, 139),
  action('web-storefront', 'act_storefront_0091', { kind: 'pull_request', number: 873, actionKind: 'review', headSHA: sha('pr873'), taskName: 'review-pr-873-f5j9w', verdict: 'passed', confidence: 'high', summary: 'loading="lazy" plus explicit dimensions avoids layout shift; LCP image correctly excluded.' }, 91),
]

// ---- Commands ----

function command(monitorName: string, id: string, fields: Omit<MonitorCommand, 'id' | 'monitorNamespace' | 'monitorName' | 'createdAt'>, createdAgo: number): MonitorCommand {
  const repo = monitors.find((m) => m.metadata.name === monitorName)?.spec.repository
  return {
    id,
    monitorNamespace: ns,
    monitorName,
    repo: `${ORG}/${repo}`,
    monitorGeneration: 1,
    createdAt: ago(createdAgo),
    processedAt: fields.status === 'processed' || fields.status === 'rejected' ? ago(Math.max(0, createdAgo - 0.2)) : undefined,
    dedupeKey: `${monitorName}:${fields.kind}:${fields.number}:${fields.intent}`,
    ...fields,
  }
}

export const commands: MonitorCommand[] = [
  command('checkout-service', 'cmd_checkout_0183', { kind: 'issue', number: 2214, source: 'github_label', label: 'orka:implement', author: 'sam.okafor', authorAssociation: 'MEMBER', permission: 'write', intent: 'implement', status: 'processed' }, 12),
  command('checkout-service', 'cmd_checkout_0181', { kind: 'pull_request', number: 1474, source: 'github_comment', command: '/orka fix-ci', commentID: '2398871142', commentURL: `${REPOS.checkout.url}/pull/1474#issuecomment-2398871142`, author: 'sam.okafor', authorAssociation: 'MEMBER', permission: 'write', intent: 'fix_ci', headSHA: sha('pr1474'), status: 'processed' }, 22),
  command('checkout-service', 'cmd_checkout_0178', { kind: 'pull_request', number: 1462, source: 'github_comment', command: '/orka review', author: 'gh-contributor-77', authorAssociation: 'CONTRIBUTOR', permission: 'read', intent: 'review', status: 'rejected', error: 'actor permission "read" is below the required "write"' }, 260),
  command('checkout-service', 'cmd_checkout_0175', { kind: 'issue', number: 2219, source: 'api', author: 'maya.chen', permission: 'admin', intent: 'plan', status: 'processed' }, 150),
  command('inventory-api', 'cmd_inventory_0042', { kind: 'issue', number: 3391, source: 'github_comment', command: '/orka triage', author: 'dev.patel', authorAssociation: 'MEMBER', permission: 'maintain', intent: 'triage', status: 'processed' }, 40),
  command('inventory-api', 'cmd_inventory_0040', { kind: 'issue', number: 3380, source: 'github_label', label: 'orka:implement', author: 'sam.okafor', authorAssociation: 'MEMBER', permission: 'write', intent: 'implement', status: 'accepted' }, 60 * 20),
]

// ---- Work actions (workflow timeline) ----

function workAction(monitorName: string, id: string, fields: Omit<MonitorWorkAction, 'id' | 'monitorNamespace' | 'monitorName' | 'createdAt' | 'updatedAt' | 'attempt'> & { attempt?: number }, createdAgo: number, updatedAgo = createdAgo): MonitorWorkAction {
  return {
    id,
    monitorNamespace: ns,
    monitorName,
    monitorGeneration: 1,
    attempt: 1,
    createdAt: ago(createdAgo),
    updatedAt: ago(updatedAgo),
    idempotencyKey: `${monitorName}:${id}`,
    ...fields,
  }
}

export const workActions: MonitorWorkAction[] = [
  workAction('checkout-service', 'wa_checkout_0443', { runID: 'run_checkout_2291', targetKind: 'pull_request', targetNumber: 1482, targetSHA: sha('pr1482'), intent: 'review', desiredAction: 'review', status: 'queued', phase: 'waiting_for_runtime', taskName: 'review-pr-1482-q8m4d' }, 1),
  workAction('checkout-service', 'wa_checkout_0441', { runID: 'run_checkout_2290', commandEventID: 'cmd_checkout_0183', targetKind: 'issue', targetNumber: 2214, intent: 'implement', desiredAction: 'implement', status: 'running', phase: 'implementing', taskName: 'fix-cart-rounding-bug-x9m4d', leaseOwner: 'orka-controller-6c9f7d8b5-x2kqm', leaseExpiresAt: ago(-4) }, 11, 1),
  workAction('checkout-service', 'wa_checkout_0439', { commandEventID: 'cmd_checkout_0181', targetKind: 'pull_request', targetNumber: 1474, targetSHA: sha('pr1474'), intent: 'fix_ci', desiredAction: 'repair', status: 'running', phase: 'validating', attempt: 2 }, 22, 3),
  workAction('checkout-service', 'wa_checkout_0436', { runID: 'run_checkout_2288', targetKind: 'pull_request', targetNumber: 1479, targetSHA: sha('pr1479'), intent: 'review', desiredAction: 'review', status: 'succeeded', phase: 'published', taskName: 'review-pr-1479-b7n2k', completedAt: ago(46) }, 53, 46),
  workAction('checkout-service', 'wa_checkout_0430', { targetKind: 'pull_request', targetNumber: 1471, targetSHA: sha('pr1471'), intent: 'merge', desiredAction: 'automerge', status: 'blocked', blockedReason: 'review verdict security_sensitive requires a human approval' }, 300, 290),
  workAction('checkout-service', 'wa_checkout_0427', { targetKind: 'issue', targetNumber: 2216, intent: 'triage', desiredAction: 'triage', status: 'succeeded', phase: 'labelled', taskName: 'triage-issue-2216-w9d3e', completedAt: ago(205) }, 210, 205),
  workAction('inventory-api', 'wa_inventory_0119', { runID: 'run_inventory_0810', commandEventID: 'cmd_inventory_0042', targetKind: 'issue', targetNumber: 3391, intent: 'triage', desiredAction: 'triage', status: 'succeeded', phase: 'labelled', taskName: 'triage-issue-3391-m2x7c', completedAt: ago(37) }, 39, 37),
  workAction('inventory-api', 'wa_inventory_0117', { commandEventID: 'cmd_inventory_0040', targetKind: 'issue', targetNumber: 3380, intent: 'implement', desiredAction: 'implement', status: 'blocked', blockedReason: 'implementation_active_budget_exhausted: maxActive 1 reached (issue #3372 in progress)' }, 60 * 20, 30),
  workAction('inventory-api', 'wa_inventory_0114', { targetKind: 'issue', targetNumber: 3388, intent: 'triage', desiredAction: 'triage', status: 'succeeded', phase: 'labelled', taskName: 'triage-issue-3388-k4v1s', completedAt: ago(139) }, 140, 139),
  workAction('web-storefront', 'wa_storefront_0048', { runID: 'run_web_0376', targetKind: 'pull_request', targetNumber: 873, targetSHA: sha('pr873'), intent: 'review', desiredAction: 'review', status: 'succeeded', phase: 'published', taskName: 'review-pr-873-f5j9w', completedAt: ago(90) }, 96, 90),
]

// ---- Implementation jobs ----

function implJob(monitorName: string, id: string, fields: Omit<MonitorImplementationJob, 'id' | 'monitorNamespace' | 'monitorName' | 'createdAt' | 'updatedAt' | 'attempt'> & { attempt?: number }, createdAgo: number, updatedAgo = createdAgo): MonitorImplementationJob {
  const repo = monitors.find((m) => m.metadata.name === monitorName)?.spec.repository
  return { id, monitorNamespace: ns, monitorName, repo: `${ORG}/${repo}`, attempt: 1, monitorGeneration: 1, createdAt: ago(createdAgo), updatedAt: ago(updatedAgo), ...fields }
}

export const implementationJobs: MonitorImplementationJob[] = [
  implJob('checkout-service', 'impl_checkout_0029', { issueNumber: 2214, phase: 'implementing', branch: 'orka/issue-2214-jpy-rounding', validationState: 'running', taskName: 'fix-cart-rounding-bug-x9m4d', commandEventID: 'cmd_checkout_0183', workActionID: 'wa_checkout_0441', snapshotDigest: `sha256:${digest('snapshot:checkout-service:2214', 16)}` }, 11, 1),
  implJob('checkout-service', 'impl_checkout_0027', { issueNumber: 2198, phase: 'pr_opened', attempt: 2, branch: 'orka/issue-2198-capture-retry-count', patchArtifactID: 'art_7f3c91a2', prNumber: 1468, validationState: 'passed', taskName: 'implement-issue-2198-h7c2x', mutationTaskName: 'publish-issue-2198-q1w8e' }, 60 * 27, 60 * 25),
  implJob('inventory-api', 'impl_inventory_0011', { issueNumber: 3372, phase: 'implementing', branch: 'orka/issue-3372-expiry-batching', validationState: 'pending', taskName: 'implement-issue-3372-v5k3t' }, 50, 4),
  implJob('inventory-api', 'impl_inventory_0009', { issueNumber: 3350, phase: 'blocked', attempt: 3, branch: 'orka/issue-3350-supplier-feed', validationState: 'failed', error: 'patch changes 34 files; maxChangedFiles is 20' }, 60 * 50, 60 * 46),
]

// ---- GitHub mutations (audit trail) ----

function mutation(monitorName: string, id: string, fields: Omit<MonitorMutation, 'id' | 'monitorNamespace' | 'monitorName' | 'createdAt'>, createdAgo: number): MonitorMutation {
  return {
    id,
    monitorNamespace: ns,
    monitorName,
    monitorGeneration: 1,
    actor: BOT,
    requestDigest: `sha256:${digest(`mutation:${id}`, 16)}`,
    githubRequestID: `${digest(`ghreq:${id}`, 4).toUpperCase()}:${digest(`ghreq2:${id}`, 6).toUpperCase()}`,
    createdAt: ago(createdAgo),
    ...fields,
  }
}

export const mutations: MonitorMutation[] = [
  mutation('checkout-service', 'mut_checkout_0902', { operation: 'push_branch', targetKind: 'pull_request', targetNumber: 1474, targetSHA: sha('pr1474-repair'), reason: 'repair: fix_ci', status: 'pending', workActionID: 'wa_checkout_0439' }, 3),
  mutation('checkout-service', 'mut_checkout_0899', { operation: 'issues', targetKind: 'issue', targetNumber: 2214, reason: 'add label orka:implementing', status: 'succeeded', githubURL: `${REPOS.checkout.url}/issues/2214` }, 11),
  mutation('checkout-service', 'mut_checkout_0896', { operation: 'submit_review', targetKind: 'pull_request', targetNumber: 1479, targetSHA: sha('pr1479'), reason: 'review verdict passed', status: 'succeeded', githubURL: reviewURL('checkout-service', 1479, 2841177390), externalID: '2841177390' }, 46),
  mutation('checkout-service', 'mut_checkout_0890', { operation: 'create_pr', targetKind: 'issue', targetNumber: 2198, reason: 'implementation ready', status: 'succeeded', githubURL: `${REPOS.checkout.url}/pull/1468`, externalID: '1468' }, 60 * 25),
  mutation('checkout-service', 'mut_checkout_0887', { operation: 'issues', targetKind: 'issue', targetNumber: 2216, reason: 'triage labels: bug, frontend, p2', status: 'succeeded', githubURL: `${REPOS.checkout.url}/issues/2216` }, 205),
  mutation('inventory-api', 'mut_inventory_0215', { operation: 'submit_review', targetKind: 'pull_request', targetNumber: 615, targetSHA: sha('pr615'), reason: 'review verdict needs_changes', status: 'succeeded', githubURL: reviewURL('inventory-api', 615, 2839981245), externalID: '2839981245' }, 27),
  mutation('inventory-api', 'mut_inventory_0211', { operation: 'issues', targetKind: 'issue', targetNumber: 3394, reason: 'add label needs-triage', status: 'failed', error: '403 Resource not accessible by integration (issues: write not granted)' }, 7),
  mutation('web-storefront', 'mut_storefront_0060', { operation: 'submit_review', targetKind: 'pull_request', targetNumber: 873, targetSHA: sha('pr873'), reason: 'review verdict passed', status: 'succeeded', githubURL: reviewURL('web-storefront', 873, 2838110984), externalID: '2838110984' }, 90),
]
