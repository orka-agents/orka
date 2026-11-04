import type { DroppedFinding, PatchProposal, RepositoryScan, ReviewSlice, ScanRun, SecurityFinding, ThreatModel } from '@/schemas/security'
import { ago, DEMO_NAMESPACE, digest, uid } from '../shared'
import { AGENTS, REPOS, type RepoKey } from '../world'

// Security scanning fixtures: four RepositoryScans on the Northwind Labs
// repositories, their scan history, review slices, threat models, findings,
// dropped model output, and patch proposals. The handlers mutate these arrays
// in place, so everything resets on page reload.

const ns = DEMO_NAMESPACE
const MINUTE = 60_000

export const SECURITY_REPOS = ['checkout-service', 'inventory-api', 'web-storefront', 'platform-infra'] as const

const repoKeyByScan: Record<string, RepoKey> = {
  'checkout-service': 'checkout',
  'inventory-api': 'inventory',
  'web-storefront': 'storefront',
  'platform-infra': 'infra',
}

export const repoURLForScan = (name: string) => {
  const key = repoKeyByScan[name]
  return key ? REPOS[key].url : undefined
}

/** A RepositoryScan without its derived status fields (counts, phase, last scan). */
export interface ScanRecord {
  resource: RepositoryScan
  /** Set when the repository is misconfigured; reported through Ready=False. */
  problem?: { reason: string; message: string }
}

function repositoryScan(
  name: string,
  createdAgo: number,
  spec: Partial<RepositoryScan['spec']>,
  status: Partial<NonNullable<RepositoryScan['status']>> = {},
): RepositoryScan {
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'RepositoryScan',
    metadata: { name, namespace: ns, uid: uid(`repositoryscan:${name}`), creationTimestamp: ago(createdAgo) },
    spec: {
      provider: 'github',
      repoURL: repoURLForScan(name) ?? '',
      branch: 'main',
      analysisAgentRef: { name: AGENTS.security.name },
      patchAgentRef: { name: AGENTS.fixer.name },
      gitSecretRef: { name: 'github-read-token' },
      validationMode: 'full',
      validationMinSeverity: 'medium',
      maxFindingsPerRun: 10,
      historyDays: 30,
      ...spec,
    },
    status: { threatModelVersion: 1, ...status },
  }
}

export const scans: ScanRecord[] = [
  {
    resource: repositoryScan('checkout-service', 60 * 24 * 41, { schedule: '0 */6 * * *', timeZone: 'America/Los_Angeles' }, {
      threatModelVersion: 4,
      lastObservedHeadSHA: digest('checkout:head'),
      lastProcessedCommit: digest('checkout:head'),
    }),
  },
  {
    resource: repositoryScan('inventory-api', 60 * 24 * 26, { schedule: '0 3 * * *', timeZone: 'Europe/Berlin', validationMinSeverity: 'high' }, {
      threatModelVersion: 2,
      lastObservedHeadSHA: digest('inventory:head'),
      lastProcessedCommit: digest('inventory:head'),
    }),
  },
  {
    resource: repositoryScan('web-storefront', 60 * 24 * 12, { schedule: '30 */12 * * *', subPath: 'apps/storefront', patchAgentRef: undefined }, {
      threatModelVersion: 1,
      lastObservedHeadSHA: digest('storefront:head'),
      lastProcessedCommit: digest('storefront:prev'),
    }),
  },
  {
    resource: repositoryScan('platform-infra', 60 * 24 * 3, { schedule: '0 6 * * 1', gitSecretRef: { name: 'github-infra-token' }, validationMode: 'off', patchAgentRef: undefined }, {
      threatModelVersion: 0,
    }),
    problem: {
      reason: 'CredentialUnavailable',
      message: 'Secret "github-infra-token" referenced by spec.gitSecretRef was not found in namespace default',
    },
  },
]

// ---- Scan runs ----

/** A scan run plus the wall-clock window it runs in; phase is derived from now. */
export interface ScanRunRecord {
  run: ScanRun
  startedMs: number
  finishMs: number
  /** Final outcome once finishMs passes. */
  outcome: 'succeeded' | 'failed'
  finalSummary: string
}

function scanRun(
  repo: string,
  seq: number,
  mode: 'incremental' | 'manual',
  startedAgoMin: number,
  durationMin: number,
  fields: Partial<ScanRun> & { outcome?: 'succeeded' | 'failed'; summary: string },
): ScanRunRecord {
  const { outcome = 'succeeded', summary, ...rest } = fields
  const startedMs = Date.now() - startedAgoMin * MINUTE
  return {
    run: {
      id: `scan_${repo.split('-')[0]}_${String(seq).padStart(3, '0')}`,
      namespace: ns,
      repositoryScan: repo,
      taskName: `security-scan-${repo.split('-')[0]}-${digest(`${repo}:${seq}`, 5)}`,
      mode,
      phase: 'running',
      startedAt: new Date(startedMs).toISOString(),
      scannerPolicyVersion: 'v2.3',
      policyDigest: `sha256:${digest(`policy:${repo}`, 16)}`,
      idempotencyKey: `${repo}:${digest(`${repo}:${seq}:key`, 12)}`,
      ...rest,
    },
    startedMs,
    finishMs: startedMs + durationMin * MINUTE,
    outcome,
    finalSummary: summary,
  }
}

export const scanRuns: ScanRunRecord[] = [
  scanRun('checkout-service', 41, 'incremental', 400, 18, {
    taskName: 'security-scan-checkout-t3y8u',
    baseCommit: digest('checkout:prev'), headCommit: digest('checkout:head'),
    commitCount: 23, sliceCount: 6, reviewedSliceCount: 5, skippedSliceCount: 1, acceptedFindings: 3, droppedFindings: 4,
    summary: 'Reviewed 5 of 6 slices across 23 new commits. Accepted 3 findings (1 critical, 1 high, 1 medium); 4 dropped by validation and filters.',
  }),
  scanRun('checkout-service', 40, 'incremental', 760, 15, {
    baseCommit: digest('checkout:prev2'), headCommit: digest('checkout:prev'),
    commitCount: 9, sliceCount: 6, reviewedSliceCount: 2, skippedSliceCount: 4, acceptedFindings: 1, droppedFindings: 1,
    summary: 'Reviewed the 2 slices touched by 9 commits. Accepted 1 medium finding.',
  }),
  scanRun('checkout-service', 39, 'manual', 1830, 22, {
    baseCommit: digest('checkout:prev3'), headCommit: digest('checkout:prev2'),
    commitCount: 41, sliceCount: 6, reviewedSliceCount: 6, skippedSliceCount: 0, acceptedFindings: 3, droppedFindings: 6,
    summary: 'Full manual review requested by lena.fischer after the refunds refactor. Accepted 3 findings.',
  }),
  scanRun('inventory-api', 18, 'incremental', 612, 11, {
    baseCommit: digest('inventory:prev'), headCommit: digest('inventory:head'),
    commitCount: 7, sliceCount: 3, reviewedSliceCount: 2, skippedSliceCount: 1, acceptedFindings: 2, droppedFindings: 2,
    summary: 'Reviewed 2 of 3 slices across 7 commits. Accepted 2 findings (1 high, 1 medium).',
  }),
  scanRun('inventory-api', 17, 'incremental', 2052, 6, {
    outcome: 'failed',
    baseCommit: digest('inventory:prev2'), headCommit: digest('inventory:prev'),
    commitCount: 3, sliceCount: 3, reviewedSliceCount: 1, skippedSliceCount: 0, acceptedFindings: 0, droppedFindings: 0,
    errorMessage: 'analysis task security-scan-inventory-0f3a1 exceeded its 30m timeout',
    summary: 'Scan failed: the analysis task timed out after reviewing 1 of 3 slices.',
  }),
  scanRun('inventory-api', 16, 'manual', 3460, 14, {
    baseCommit: digest('inventory:prev3'), headCommit: digest('inventory:prev2'),
    commitCount: 52, sliceCount: 3, reviewedSliceCount: 3, skippedSliceCount: 0, acceptedFindings: 2, droppedFindings: 3,
    summary: 'Initial full review. Accepted 2 findings; threat model v2 generated.',
  }),
  // In progress when the page loads; finishes about two minutes later.
  scanRun('web-storefront', 8, 'incremental', 3, 5, {
    baseCommit: digest('storefront:prev'), headCommit: digest('storefront:head'),
    commitCount: 12, sliceCount: 2,
    acceptedFindings: 0, droppedFindings: 1,
    summary: 'Reviewed 2 slices across 12 commits. No new findings; 1 candidate dropped as a duplicate.',
  }),
  scanRun('web-storefront', 7, 'incremental', 725, 9, {
    baseCommit: digest('storefront:prev2'), headCommit: digest('storefront:prev'),
    commitCount: 18, sliceCount: 2, reviewedSliceCount: 2, skippedSliceCount: 0, acceptedFindings: 3, droppedFindings: 2,
    summary: 'Reviewed 2 slices across 18 commits. Accepted 3 findings (1 high, 1 medium, 1 low).',
  }),
]

/** Current view of a scan run: running until finishMs, then its outcome. */
export function viewScanRun(record: ScanRunRecord, nowMs = Date.now()): ScanRun {
  if (nowMs < record.finishMs) {
    const progress = Math.min(1, Math.max(0, (nowMs - record.startedMs) / (record.finishMs - record.startedMs)))
    const sliceCount = record.run.sliceCount ?? 0
    return {
      ...record.run,
      phase: 'running',
      completedAt: undefined,
      reviewedSliceCount: Math.floor(sliceCount * progress),
      acceptedFindings: undefined,
      droppedFindings: undefined,
      errorMessage: undefined,
      summary: `scan is running (${Math.floor(sliceCount * progress)} of ${sliceCount} slices reviewed)`,
    }
  }
  return {
    ...record.run,
    phase: record.outcome,
    completedAt: new Date(record.finishMs).toISOString(),
    reviewedSliceCount: record.run.reviewedSliceCount ?? record.run.sliceCount,
    summary: record.finalSummary,
  }
}

// ---- Threat models ----

export const threatModels: ThreatModel[] = [
  {
    namespace: ns,
    repositoryScan: 'checkout-service',
    version: 4,
    source: 'edited',
    generatedByScan: 'scan_checkout_039',
    createdAt: ago(60 * 24 * 41),
    updatedAt: ago(60 * 24 * 2),
    content: `# checkout-service threat model

## Assets
- Payment intents, captures, and refunds (Stripe + Adyen)
- Merchant API keys and webhook signing secrets
- Order history and customer PII (email, shipping address, last-4)

## Trust boundaries
1. Public storefront -> /v2/checkout (session cookie, CSRF token)
2. Merchant dashboard -> /merchant/* (OIDC, merchant_id claim)
3. Payment processor webhooks -> /webhooks/{stripe,adyen} (HMAC signature)
4. Outbox relay -> Kafka \`order-events\` topic (mTLS)

## Authorization assumptions
- Every merchant-scoped query MUST filter by the merchant_id from the token, never from the request body.
- Refunds require the \`refunds:write\` scope and are capped at the captured amount.
- Internal admin routes are only reachable through the ops VPN.

## Priority attack surfaces
- Refund and dispute handlers (money movement, IDOR risk)
- Settlement report filters (dynamic SQL)
- Webhook endpoint preview (outbound requests to merchant URLs)
- Logging on payment failures (PII leakage)

## Out of scope
- Stripe/Adyen hosted fields (PCI scope handled by the processors)
- Load-shedding and rate limits (owned by the API gateway team)
`,
  },
  {
    namespace: ns,
    repositoryScan: 'inventory-api',
    version: 2,
    source: 'generated',
    generatedByScan: 'scan_inventory_016',
    createdAt: ago(60 * 24 * 26),
    updatedAt: ago(3460),
    content: `# inventory-api threat model

## Assets
- Stock levels and reservations for ~180k SKUs
- Supplier price lists (confidential)
- CSV exports written to the shared exports volume

## Trust boundaries
1. Internal services -> REST API (service-to-service JWT)
2. Warehouse scanners -> /scan endpoints (device certificates)
3. Redis reservation cache (shared with the pricing service)

## Key risks
- Concurrent reservations racing on the same SKU row
- File paths built from user-supplied export names
- Deserializing cached payloads written by other services
- Bulk endpoints that skip per-warehouse authorization
`,
  },
  {
    namespace: ns,
    repositoryScan: 'web-storefront',
    version: 1,
    source: 'generated',
    generatedByScan: 'scan_web_007',
    createdAt: ago(60 * 24 * 12),
    updatedAt: ago(60 * 24 * 12),
    content: `# web-storefront threat model

## Assets
- Customer sessions and the checkout cart
- Search, product, and promotion content rendered from the CMS

## Trust boundaries
1. Browser -> Next.js server components and API routes
2. Next.js -> checkout-service and inventory-api (internal network)
3. CMS content -> rendered HTML (editors are semi-trusted)

## Priority attack surfaces
- Any HTML built from query parameters or CMS fields (XSS)
- Post-login redirects (open redirect)
- Values inlined into the client bundle at build time
`,
  },
]

// ---- Review slices ----

function slice(repo: string, id: string, fields: Omit<ReviewSlice, 'id' | 'repositoryScan' | 'namespace' | 'source' | 'confidence' | 'status'> & Partial<Pick<ReviewSlice, 'confidence' | 'status'>>, reviewedAgo: number, lastScanRunID: string): ReviewSlice {
  return {
    schemaVersion: 1,
    id,
    namespace: ns,
    repositoryScan: repo,
    source: 'mapper',
    confidence: 'high',
    status: 'reviewed',
    lastScanRunID,
    lastReviewedAt: ago(reviewedAgo),
    createdAt: ago(60 * 24 * 20),
    updatedAt: ago(reviewedAgo),
    ...fields,
  }
}

export const slices: ReviewSlice[] = [
  slice('checkout-service', 'slice_checkout_refunds', {
    title: 'Refunds and disputes API', kind: 'route', summary: 'Merchant-facing refund creation, lookup, and dispute evidence upload.',
    entrypoints: [{ path: 'internal/http/refunds.go', route: 'POST /merchant/v1/refunds', symbol: 'RefundHandler.Create' }],
    ownedFiles: [{ path: 'internal/http/refunds.go' }, { path: 'internal/refunds/service.go' }, { path: 'internal/refunds/store.go' }],
    contextFiles: [{ path: 'internal/auth/merchant.go', reason: 'merchant_id claim extraction' }],
    tests: [{ path: 'internal/http/refunds_test.go', command: 'go test ./internal/http -run TestRefund' }],
    tags: ['money-movement', 'authz'], trustBoundaries: ['merchant dashboard -> API'],
  }, 382, 'scan_checkout_041'),
  slice('checkout-service', 'slice_checkout_reports', {
    title: 'Settlement reports', kind: 'package', summary: 'Merchant settlement CSV and JSON reports with filtering and sorting.',
    entrypoints: [{ path: 'internal/reports/handler.go', route: 'GET /merchant/v1/reports/settlements' }],
    ownedFiles: [{ path: 'internal/reports/settlement.go' }, { path: 'internal/reports/handler.go' }],
    tests: [{ path: 'internal/reports/settlement_test.go', command: 'go test ./internal/reports' }],
    tags: ['sql'], trustBoundaries: ['merchant dashboard -> API'],
  }, 382, 'scan_checkout_041'),
  slice('checkout-service', 'slice_checkout_webhooks', {
    title: 'Merchant webhook endpoints', kind: 'route', summary: 'Merchant-configured webhook URLs, test deliveries, and endpoint preview.',
    entrypoints: [{ path: 'internal/webhooks/preview.go', route: 'POST /merchant/v1/webhooks/preview' }],
    ownedFiles: [{ path: 'internal/webhooks/preview.go' }, { path: 'internal/webhooks/deliver.go' }],
    tags: ['ssrf', 'outbound-http'], trustBoundaries: ['API -> merchant URLs'],
  }, 382, 'scan_checkout_041'),
  slice('checkout-service', 'slice_checkout_capture', {
    title: 'Payment capture', kind: 'package', summary: 'Capture, retry, and failure handling against Stripe and Adyen.',
    ownedFiles: [{ path: 'internal/payments/capture.go' }, { path: 'internal/payments/idempotency.go' }],
    contextFiles: [{ path: 'internal/payments/stripe/client.go' }],
    tags: ['pii', 'money-movement'],
  }, 382, 'scan_checkout_041'),
  slice('checkout-service', 'slice_checkout_dashboard', {
    title: 'Merchant settings forms', kind: 'route', summary: 'Server-rendered merchant settings pages.',
    entrypoints: [{ path: 'web/templates/settings.html' }, { path: 'internal/http/settings.go', route: 'POST /merchant/settings' }],
    tags: ['csrf'], confidence: 'medium',
  }, 760, 'scan_checkout_040'),
  slice('checkout-service', 'slice_checkout_outbox', {
    title: 'Order events outbox relay', kind: 'package', summary: 'Transactional outbox and Kafka relay worker.',
    ownedFiles: [{ path: 'internal/outbox/relay.go' }, { path: 'internal/outbox/store.go' }],
    status: 'skipped', confidence: 'medium',
  }, 382, 'scan_checkout_041'),
  slice('inventory-api', 'slice_inventory_exports', {
    title: 'CSV exports', kind: 'route', summary: 'Export generation and download endpoints.',
    entrypoints: [{ path: 'app/routes/exports.py', route: 'GET /v1/exports/{name}' }],
    ownedFiles: [{ path: 'app/routes/exports.py' }, { path: 'app/exports/writer.py' }],
    tests: [{ path: 'tests/routes/test_exports.py', command: 'pytest tests/routes/test_exports.py' }],
    tags: ['filesystem'],
  }, 601, 'scan_inventory_018'),
  slice('inventory-api', 'slice_inventory_reservations', {
    title: 'Reservations', kind: 'package', summary: 'Stock reservation, expiry, and the Redis reservation cache.',
    ownedFiles: [{ path: 'app/reservations/service.py' }, { path: 'app/reservations/cache.py' }],
    tests: [{ path: 'tests/reservations/test_service.py' }],
    tags: ['concurrency', 'deserialization'],
  }, 601, 'scan_inventory_018'),
  slice('inventory-api', 'slice_inventory_bulk', {
    title: 'Bulk SKU operations', kind: 'route', summary: 'Bulk stock adjustments and SKU import.',
    entrypoints: [{ path: 'app/routes/bulk.py', route: 'POST /v1/skus/bulk-adjust' }],
    tags: ['authz'], status: 'pending', confidence: 'medium',
  }, 3460, 'scan_inventory_016'),
  slice('web-storefront', 'slice_storefront_search', {
    title: 'Search results', kind: 'route', summary: 'Search page and the result highlighting component.',
    entrypoints: [{ path: 'apps/storefront/src/app/search/page.tsx', route: '/search' }],
    ownedFiles: [{ path: 'apps/storefront/src/components/search/Highlight.tsx' }],
    tags: ['xss'],
  }, 716, 'scan_web_007'),
  slice('web-storefront', 'slice_storefront_auth', {
    title: 'Login and session', kind: 'route', summary: 'Login page, OAuth callback, and post-login redirect.',
    entrypoints: [{ path: 'apps/storefront/src/app/login/page.tsx', route: '/login' }],
    tags: ['redirect', 'session'],
  }, 716, 'scan_web_007'),
]

// ---- Findings ----

type FindingSeed = Omit<SecurityFinding, 'namespace' | 'fingerprint' | 'createdAt' | 'updatedAt' | 'id'> & { createdAgo: number; updatedAgo?: number; key: string }

/** Stable finding ID for a fixture key. */
export const findingID = (key: string) => `fnd_${digest(`finding:${key}`, 12)}`

function finding(seed: FindingSeed): SecurityFinding {
  const { createdAgo, updatedAgo, key, ...rest } = seed
  return {
    id: findingID(key),
    namespace: ns,
    fingerprint: `sha256:${digest(`fingerprint:${key}`, 24)}`,
    createdAt: ago(createdAgo),
    updatedAt: ago(updatedAgo ?? createdAgo),
    ...rest,
  }
}

const validation = (fields: { summary: string; steps: string[]; attackPath: string; assumptions?: string[]; controls?: string[]; blindspots?: string[] }) =>
  JSON.stringify({
    summary: fields.summary,
    validation_steps: fields.steps,
    attack_path_analysis: fields.attackPath,
    // Omitted lists stay absent so the dashboard does not render empty columns.
    assumptions: fields.assumptions,
    controls: fields.controls,
    blindspots: fields.blindspots,
  })

export const findings: SecurityFinding[] = [
  finding({
    key: 'checkout-refund-idor',
    repositoryScan: 'checkout-service', scanRunID: 'scan_checkout_039', scanTaskName: 'security-scan-checkout-9c2e1', sliceID: 'slice_checkout_refunds',
    title: 'Refund lookup is not scoped to the calling merchant (IDOR)',
    category: 'cwe-639',
    summary: 'POST /merchant/v1/refunds loads the order by ID without filtering on the merchant_id claim, so any merchant can refund another merchant\'s captured payment.',
    severity: 'critical', confidence: 'high', triage: 'confirmed', validationStatus: 'validated', state: 'pr_open',
    filePath: 'internal/http/refunds.go', line: 88, commitSHA: digest('checkout:prev2'),
    rootCause: 'RefundHandler.Create calls orders.Get(ctx, req.OrderID) and only checks that the order exists. The merchant ID from the token is used for audit logging but never compared with order.MerchantID.',
    reproduction: 'Sign in as merchant A, capture an order as merchant B, then POST {"order_id": "<B\'s order>", "amount": 1999} to /merchant/v1/refunds with A\'s token. The refund succeeds and is credited to B\'s customer from B\'s balance.',
    remediation: 'Load orders through a merchant-scoped query (orders.GetForMerchant) and return 404 when the order belongs to another merchant, so existence is not leaked either.',
    suggestedAction: 'Merge the generated patch in #1486 and backfill an audit query for cross-merchant refunds in the last 90 days.',
    whyTestsDoNotAlreadyCoverThis: 'refunds_test.go only exercises a single merchant fixture, so the cross-tenant path is never taken.',
    suggestedRegressionTest: 'TestRefundCreate_OtherMerchantsOrder_Returns404 in internal/http/refunds_test.go with two merchant fixtures.',
    minimumFixScope: 'internal/http/refunds.go and internal/refunds/store.go (add GetForMerchant).',
    evidence: [
      { kind: 'code', path: 'internal/http/refunds.go', startLine: 84, endLine: 92, symbol: 'RefundHandler.Create', quote: 'order, err := h.orders.Get(ctx, req.OrderID)\nif err != nil {\n    return httpError(w, err)\n}\n// merchantID only used for audit\nh.audit.Record(ctx, merchantID, "refund.create", order.ID)' },
      { kind: 'code', path: 'internal/refunds/store.go', startLine: 41, endLine: 44, symbol: 'Store.Get', quote: 'SELECT id, merchant_id, amount_captured FROM orders WHERE id = $1' },
    ],
    validationJSON: validation({
      summary: 'Reproduced against a local stack: merchant A refunded 19.99 USD on merchant B\'s order.',
      steps: ['Seeded two merchants with one captured order each', 'Called POST /merchant/v1/refunds as merchant A with merchant B\'s order ID', 'Observed 201 Created and a refund row owned by merchant B'],
      attackPath: 'Any authenticated merchant (self-serve signup) -> enumerate sequential order IDs from /v2/checkout receipts -> issue refunds against other merchants\' balances.',
      assumptions: ['Order IDs are guessable (ULIDs leak creation time)'],
      controls: ['Refund amount is capped at the captured amount'],
      blindspots: ['Dispute evidence upload uses the same lookup and was not exercised'],
    }),
    patchProposalID: 'patch_refund_scope', prNumber: 1486, prURL: `${REPOS.checkout.url}/pull/1486`,
    createdAgo: 1810, updatedAgo: 95,
  }),
  finding({
    key: 'checkout-report-sqli',
    repositoryScan: 'checkout-service', scanRunID: 'scan_checkout_041', scanTaskName: 'security-scan-checkout-t3y8u', sliceID: 'slice_checkout_reports',
    title: 'SQL injection through the settlement report sort parameter',
    category: 'cwe-89',
    summary: 'The `sort` query parameter is interpolated into ORDER BY, letting a merchant run arbitrary SQL against the reports replica.',
    severity: 'high', confidence: 'high', triage: 'confirmed', validationStatus: 'validated', state: 'patch_ready',
    filePath: 'internal/reports/settlement.go', line: 142, commitSHA: digest('checkout:head'),
    rootCause: 'buildSettlementQuery appends fmt.Sprintf(" ORDER BY %s %s", sort, dir) after validating only `dir`.',
    reproduction: 'GET /merchant/v1/reports/settlements?sort=(SELECT CASE WHEN (SELECT count(*) FROM merchants)>0 THEN created_at ELSE amount END) returns rows in a different order, confirming boolean-based injection.',
    remediation: 'Map the sort parameter through an allow-list of column names and reject anything else with 400.',
    suggestedRegressionTest: 'Table test in settlement_test.go that every non-allow-listed sort value is rejected.',
    minimumFixScope: 'internal/reports/settlement.go',
    evidence: [
      { kind: 'code', path: 'internal/reports/settlement.go', startLine: 138, endLine: 144, symbol: 'buildSettlementQuery', quote: 'if dir != "asc" && dir != "desc" {\n    dir = "desc"\n}\nq += fmt.Sprintf(" ORDER BY %s %s", sort, dir)' },
      { kind: 'artifact', taskName: 'security-scan-checkout-t3y8u', name: 'semgrep.sarif.json', label: 'Semgrep SARIF report' },
    ],
    validationJSON: validation({
      summary: 'Boolean-based injection confirmed on the reports replica; the replica user can read every merchant\'s settlements.',
      steps: ['Requested the report with a CASE expression in sort', 'Compared row order for true and false predicates', 'Extracted the merchants table row count'],
      attackPath: 'Authenticated merchant -> crafted sort parameter -> read other merchants\' settlement data from the shared replica.',
      controls: ['Replica user is read-only'],
    }),
    patchProposalID: 'patch_report_sort',
    createdAgo: 382, updatedAgo: 41,
  }),
  finding({
    key: 'checkout-webhook-ssrf',
    repositoryScan: 'checkout-service', scanRunID: 'scan_checkout_041', scanTaskName: 'security-scan-checkout-t3y8u', sliceID: 'slice_checkout_webhooks',
    title: 'Webhook endpoint preview can reach internal services (SSRF)',
    category: 'cwe-918',
    summary: 'The preview endpoint fetches any merchant-supplied URL server-side, including link-local and cluster-internal addresses.',
    severity: 'high', confidence: 'medium', validationStatus: 'unvalidated', state: 'open',
    filePath: 'internal/webhooks/preview.go', line: 57, commitSHA: digest('checkout:head'),
    rootCause: 'Preview uses http.DefaultClient with no destination filtering; redirects are followed.',
    remediation: 'Route previews through the egress proxy used for real deliveries, which already blocks RFC 1918, link-local, and cluster CIDRs.',
    suggestedAction: 'Run validation to confirm whether the metadata endpoint is reachable from the checkout pods.',
    evidence: [
      { kind: 'code', path: 'internal/webhooks/preview.go', startLine: 52, endLine: 60, symbol: 'PreviewHandler.ServeHTTP', quote: 'resp, err := http.DefaultClient.Get(req.URL)\nif err != nil {\n    return previewError(w, err)\n}\ndefer resp.Body.Close()\nbody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))' },
    ],
    createdAgo: 382,
  }),
  finding({
    key: 'checkout-pii-logs',
    repositoryScan: 'checkout-service', scanRunID: 'scan_checkout_041', scanTaskName: 'security-scan-checkout-t3y8u', sliceID: 'slice_checkout_capture',
    title: 'Customer email and card fingerprint logged on capture failure',
    category: 'cwe-532',
    summary: 'Failed captures log the full payment intent, including customer email and card fingerprint, at ERROR level to the shared log pipeline.',
    severity: 'medium', confidence: 'high', triage: 'confirmed', validationStatus: 'validated', state: 'open',
    filePath: 'internal/payments/capture.go', line: 203, commitSHA: digest('checkout:head'),
    rootCause: 'log.Error("capture failed", "intent", intent) serializes the whole struct.',
    remediation: 'Log the intent ID and processor error code only; add a redacting LogValue method to PaymentIntent.',
    evidence: [{ kind: 'code', path: 'internal/payments/capture.go', startLine: 201, endLine: 204, quote: 'if err != nil {\n    log.Error("capture failed", "intent", intent, "err", err)' }],
    validationJSON: validation({
      summary: 'Forced a declined capture in the sandbox and found the customer email in the log line.',
      steps: ['Captured with Stripe test card 4000 0000 0000 0002', 'Searched the checkout logs for the customer email'],
      attackPath: 'Anyone with read access to the logging platform (≈140 engineers) can collect customer emails and card fingerprints.',
    }),
    createdAgo: 382, updatedAgo: 300,
  }),
  finding({
    key: 'checkout-settings-csrf',
    repositoryScan: 'checkout-service', scanRunID: 'scan_checkout_040', scanTaskName: 'security-scan-checkout-4b7d0', sliceID: 'slice_checkout_dashboard',
    title: 'Merchant settings form accepts cross-site POSTs (missing CSRF token)',
    category: 'cwe-352',
    summary: 'POST /merchant/settings relies on the session cookie alone; the payout account can be changed from a malicious page.',
    severity: 'medium', confidence: 'medium', validationStatus: 'pending', state: 'open',
    filePath: 'internal/http/settings.go', line: 34, commitSHA: digest('checkout:prev'),
    rootCause: 'The settings router is mounted outside the csrf.Protect middleware group.',
    remediation: 'Mount the settings routes under the CSRF-protected group and set SameSite=Strict on the merchant session cookie.',
    evidence: [{ kind: 'code', path: 'internal/http/router.go', startLine: 71, endLine: 73, quote: 'r.Post("/merchant/settings", settings.Update) // TODO: move under protected group' }],
    createdAgo: 745,
  }),
  finding({
    key: 'checkout-idempotency-timing',
    repositoryScan: 'checkout-service', scanRunID: 'scan_checkout_040', scanTaskName: 'security-scan-checkout-4b7d0', sliceID: 'slice_checkout_capture',
    title: 'Idempotency keys compared with non-constant-time equality',
    category: 'cwe-208',
    summary: 'Idempotency key lookups use == on the raw key, which could in theory leak key prefixes through timing.',
    severity: 'low', confidence: 'low', validationStatus: 'failed', state: 'open',
    filePath: 'internal/payments/idempotency.go', line: 29, commitSHA: digest('checkout:prev'),
    remediation: 'Hash keys before lookup (keys are already stored hashed in the database).',
    validationJSON: validation({
      summary: 'Not reproducible: keys are looked up by hash in Postgres, so the in-memory comparison never sees attacker-controlled prefixes.',
      steps: ['Measured 20k lookups with matching and non-matching prefixes', 'No statistically significant timing difference'],
      attackPath: 'None found.',
    }),
    createdAgo: 745, updatedAgo: 700,
  }),
  finding({
    key: 'checkout-debug-traces',
    repositoryScan: 'checkout-service', scanRunID: 'scan_checkout_039', scanTaskName: 'security-scan-checkout-9c2e1',
    title: 'Stack traces returned in 500 responses when DEBUG is set',
    category: 'cwe-209',
    summary: 'The error middleware includes the stack trace in the response body when DEBUG=true.',
    severity: 'medium', confidence: 'medium', validationStatus: 'skipped', state: 'dismissed',
    filePath: 'internal/http/errors.go', line: 18,
    remediation: 'DEBUG is never set in staging or production manifests; dismissed by dev.patel.',
    createdAgo: 1810, updatedAgo: 1500,
  }),
  finding({
    key: 'inventory-export-traversal',
    repositoryScan: 'inventory-api', scanRunID: 'scan_inventory_018', scanTaskName: 'security-scan-inventory-7e1c4', sliceID: 'slice_inventory_exports',
    title: 'Path traversal in CSV export download',
    category: 'cwe-22',
    summary: 'GET /v1/exports/{name} joins the name onto the exports directory without normalization, so ../ sequences read arbitrary files.',
    severity: 'high', confidence: 'high', triage: 'confirmed', validationStatus: 'validated', state: 'patch_pending',
    filePath: 'app/routes/exports.py', line: 41, commitSHA: digest('inventory:head'),
    rootCause: 'os.path.join(EXPORT_DIR, name) is passed straight to FileResponse.',
    reproduction: 'GET /v1/exports/..%2f..%2fapp%2fsettings.py returns the settings module, including the Redis URL.',
    remediation: 'Resolve the path and require it to stay under EXPORT_DIR (Path.resolve().is_relative_to), or look exports up by ID in the database.',
    suggestedRegressionTest: 'test_export_download_rejects_traversal in tests/routes/test_exports.py.',
    evidence: [{ kind: 'code', path: 'app/routes/exports.py', startLine: 38, endLine: 42, symbol: 'download_export', quote: '@router.get("/v1/exports/{name}")\nasync def download_export(name: str):\n    path = os.path.join(EXPORT_DIR, name)\n    return FileResponse(path)' }],
    validationJSON: validation({
      summary: 'Read app/settings.py through the export endpoint with a service token.',
      steps: ['Requested /v1/exports/..%2f..%2fapp%2fsettings.py', 'Received 200 with the module source'],
      attackPath: 'Any internal service with an inventory JWT -> read configuration and credentials from the container filesystem.',
    }),
    patchProposalID: 'patch_export_path',
    createdAgo: 601, updatedAgo: 12,
  }),
  finding({
    key: 'inventory-pickle-cache',
    repositoryScan: 'inventory-api', scanRunID: 'scan_inventory_018', scanTaskName: 'security-scan-inventory-7e1c4', sliceID: 'slice_inventory_reservations',
    title: 'Reservation cache deserializes pickled payloads from shared Redis',
    category: 'cwe-502',
    summary: 'Cached reservations are stored with pickle in a Redis instance shared with the pricing service; whoever can write that key prefix gets code execution in inventory-api.',
    severity: 'high', confidence: 'medium', validationStatus: 'unvalidated', state: 'open',
    filePath: 'app/reservations/cache.py', line: 23, commitSHA: digest('inventory:head'),
    remediation: 'Serialize reservations as JSON (msgspec) and give inventory-api its own Redis ACL user with a key prefix.',
    evidence: [{ kind: 'code', path: 'app/reservations/cache.py', startLine: 21, endLine: 24, quote: 'raw = await redis.get(f"resv:{sku}")\nif raw:\n    return pickle.loads(raw)' }],
    createdAgo: 601,
  }),
  finding({
    key: 'inventory-negative-stock',
    repositoryScan: 'inventory-api', scanRunID: 'scan_inventory_016', scanTaskName: 'security-scan-inventory-2a9b8', sliceID: 'slice_inventory_reservations',
    title: 'Concurrent reservations can drive stock negative (TOCTOU)',
    category: 'cwe-367',
    summary: 'reserve() reads available stock and writes the decrement in separate statements without a row lock; matches issue #3391.',
    severity: 'medium', confidence: 'high', triage: 'confirmed', validationStatus: 'validated', state: 'open',
    filePath: 'app/reservations/service.py', line: 77, commitSHA: digest('inventory:prev2'),
    remediation: 'Use a single UPDATE ... SET available = available - :qty WHERE sku = :sku AND available >= :qty and check the row count.',
    validationJSON: validation({
      summary: '50 concurrent reservations of the last unit produced 7 successful reservations and available = -6.',
      steps: ['Seeded SKU with available=1', 'Fired 50 concurrent POST /v1/reservations', 'Counted successes and final stock'],
      attackPath: 'Not a confidentiality issue; enables overselling during flash sales.',
    }),
    createdAgo: 3446, updatedAgo: 38,
  }),
  finding({
    key: 'inventory-bulk-authz',
    repositoryScan: 'inventory-api', scanRunID: 'scan_inventory_016', scanTaskName: 'security-scan-inventory-2a9b8', sliceID: 'slice_inventory_bulk',
    title: 'Bulk stock adjustment skips per-warehouse authorization',
    category: 'cwe-862',
    summary: 'POST /v1/skus/bulk-adjust checks the caller\'s service role but not the warehouse_id on each row.',
    severity: 'medium', confidence: 'medium', validationStatus: 'unvalidated', state: 'open',
    filePath: 'app/routes/bulk.py', line: 64,
    remediation: 'Apply authorize_warehouse() per row, as the single-SKU endpoint does.',
    createdAgo: 3446,
  }),
  finding({
    key: 'storefront-search-xss',
    repositoryScan: 'web-storefront', scanRunID: 'scan_web_007', scanTaskName: 'security-scan-web-c84d2', sliceID: 'slice_storefront_search',
    title: 'Reflected XSS in search result highlighting',
    category: 'cwe-79',
    summary: 'Highlight wraps matches of the raw query in <mark> and renders with dangerouslySetInnerHTML without escaping the query.',
    severity: 'high', confidence: 'high', triage: 'confirmed', validationStatus: 'validated', state: 'open',
    filePath: 'apps/storefront/src/components/search/Highlight.tsx', line: 23, commitSHA: digest('storefront:prev'),
    reproduction: 'Open /search?q=<img src=x onerror=alert(document.domain)> and observe the alert.',
    remediation: 'Split the text on the escaped query and render <mark> elements as React children instead of building HTML.',
    evidence: [{ kind: 'code', path: 'apps/storefront/src/components/search/Highlight.tsx', startLine: 20, endLine: 24, quote: 'const html = text.replace(new RegExp(query, "gi"), (m) => `<mark>${m}</mark>`)\nreturn <span dangerouslySetInnerHTML={{ __html: html }} />' }],
    validationJSON: validation({
      summary: 'Payload executes in Chrome and Firefox; session cookie is HttpOnly so impact is limited to actions as the user.',
      steps: ['Visited the crafted search URL in a fresh session', 'Observed script execution'],
      attackPath: 'Phishing link -> script runs on the storefront origin -> add items or change the shipping address in the victim\'s cart.',
      controls: ['Session cookie is HttpOnly', 'No CSP header today'],
    }),
    createdAgo: 716, updatedAgo: 600,
  }),
  finding({
    key: 'storefront-open-redirect',
    repositoryScan: 'web-storefront', scanRunID: 'scan_web_007', scanTaskName: 'security-scan-web-c84d2', sliceID: 'slice_storefront_auth',
    title: 'Open redirect through the returnTo parameter after login',
    category: 'cwe-601',
    summary: 'The login page redirects to any absolute URL in returnTo.',
    severity: 'medium', confidence: 'high', validationStatus: 'unvalidated', state: 'open',
    filePath: 'apps/storefront/src/app/login/page.tsx', line: 58,
    remediation: 'Only accept same-origin relative paths that start with a single "/".',
    createdAgo: 716,
  }),
  finding({
    key: 'storefront-publishable-key',
    repositoryScan: 'web-storefront', scanRunID: 'scan_web_007', scanTaskName: 'security-scan-web-c84d2',
    title: 'Stripe publishable key embedded in the client bundle',
    category: 'cwe-200',
    summary: 'NEXT_PUBLIC_STRIPE_KEY is inlined into the JavaScript bundle.',
    severity: 'low', confidence: 'low', validationStatus: 'skipped', state: 'dismissed',
    filePath: 'apps/storefront/next.config.mjs', line: 12,
    remediation: 'Publishable keys are designed to be public; dismissed as a false positive.',
    createdAgo: 716, updatedAgo: 650,
  }),
]

/** States that no longer count as open work. */
export const CLOSED_FINDING_STATES = new Set(['dismissed', 'suppressed', 'false_positive', 'fixed', 'resolved'])

// ---- Patch proposals ----

export interface PatchRecord {
  proposal: PatchProposal
  /** Patch generation finishes at this time; until then the proposal is running. */
  readyMs?: number
}

export const patches: PatchRecord[] = [
  {
    proposal: {
      id: 'patch_refund_scope', namespace: ns, repositoryScan: 'checkout-service', findingID: findingID('checkout-refund-idor'),
      taskName: 'security-patch-refund-scope-m3k8q', branch: 'orka/security/refund-merchant-scope',
      diffArtifact: 'patch.diff', summaryArtifact: 'patch-summary.md',
      status: 'pr_opened', prNumber: 1486, prURL: `${REPOS.checkout.url}/pull/1486`,
      createdAt: ago(140), updatedAt: ago(95),
    },
  },
  {
    proposal: {
      id: 'patch_report_sort', namespace: ns, repositoryScan: 'checkout-service', findingID: findingID('checkout-report-sqli'),
      taskName: 'security-patch-report-sort-x2d7f', branch: 'orka/security/settlement-sort-allowlist',
      diffArtifact: 'patch.diff', summaryArtifact: 'patch-summary.md',
      status: 'succeeded', createdAt: ago(55), updatedAt: ago(41),
    },
  },
  {
    proposal: {
      id: 'patch_export_path', namespace: ns, repositoryScan: 'inventory-api', findingID: findingID('inventory-export-traversal'),
      taskName: 'security-patch-export-path-p6v1n', branch: 'orka/security/export-path-confinement',
      status: 'running', createdAt: ago(12), updatedAt: ago(12),
    },
    readyMs: Date.now() + 90_000,
  },
  {
    proposal: {
      id: 'patch_export_path_v1', namespace: ns, repositoryScan: 'inventory-api', findingID: findingID('inventory-export-traversal'),
      taskName: 'security-patch-export-path-b0r4s', branch: 'orka/security/export-path-confinement',
      status: 'failed', reason: 'validation failed: tests/routes/test_exports.py::test_download_nested_export expected 200, got 400',
      createdAt: ago(180), updatedAt: ago(165),
    },
  },
]

// ---- Dropped findings ----

function dropped(repo: string, scanRunID: string, taskName: string, n: number, layer: string, reason: string, createdAgo: number, sliceID?: string): DroppedFinding {
  return {
    id: `drop_${scanRunID.replace('scan_', '')}_${n}`,
    namespace: ns,
    repositoryScan: repo,
    scanRunID,
    taskName,
    sliceID,
    layer,
    reason,
    sampleJSON: JSON.stringify({ title: reason.split(':')[0], severity: 'medium' }),
    createdAt: ago(createdAgo),
  }
}

export const droppedFindings: DroppedFinding[] = [
  dropped('checkout-service', 'scan_checkout_041', 'security-scan-checkout-t3y8u', 1, 'validation', 'evidence quote does not match cited file range', 384, 'slice_checkout_outbox'),
  dropped('checkout-service', 'scan_checkout_041', 'security-scan-checkout-t3y8u', 2, 'validation', 'cited file internal/payments/refund_v1.go does not exist at head', 384, 'slice_checkout_refunds'),
  dropped('checkout-service', 'scan_checkout_041', 'security-scan-checkout-t3y8u', 3, 'filter', 'duplicate of an open finding (same fingerprint)', 383, 'slice_checkout_refunds'),
  dropped('checkout-service', 'scan_checkout_041', 'security-scan-checkout-t3y8u', 4, 'filter', 'matches the false-positive policy: test fixtures', 383),
  dropped('inventory-api', 'scan_inventory_018', 'security-scan-inventory-7e1c4', 1, 'validation', 'severity "critical" not supported by evidence', 603, 'slice_inventory_exports'),
  dropped('inventory-api', 'scan_inventory_018', 'security-scan-inventory-7e1c4', 2, 'cap', 'maxFindingsPerRun limit 10 reached', 602),
  dropped('web-storefront', 'scan_web_008', 'security-scan-web-a19f3', 1, 'filter', 'duplicate of an open finding (same fingerprint)', 1, 'slice_storefront_search'),
  dropped('web-storefront', 'scan_web_007', 'security-scan-web-c84d2', 1, 'validation', 'evidence quote does not match cited file range', 718),
  dropped('web-storefront', 'scan_web_007', 'security-scan-web-c84d2', 2, 'filter', 'below validationMinSeverity medium', 717),
]
