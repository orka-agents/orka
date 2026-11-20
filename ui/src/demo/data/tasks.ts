import { isAutonomousAgent } from './agents'
import { currentRuntimePool } from './runtimes'
import { digest } from '../shared'
import { AGENTS, REPOS, type AgentKey, type PEOPLE, type RepoKey } from '../world'

// Scripted histories for the demo's tasks. Each task plays a story: a list of
// beats (execution events, log lines, approvals, artifacts) spread over the
// task's run time, then an outcome. The engine in handlers/tasks.ts places the
// beats on the clock, so finished tasks show a complete history at page load
// and running tasks keep progressing while the page is open.

export type Severity = 'debug' | 'info' | 'warning' | 'error'

export interface EventSpec {
  type: string
  severity?: Severity
  summary?: string
  toolName?: string
  toolCallID?: string
  provider?: string
  model?: string
  stopReason?: string
  inputTokens?: number
  outputTokens?: number
  cachedInputTokens?: number
  contextWindowUsed?: number
  contextWindowSize?: number
  content?: unknown
  contentText?: string
}

export interface ApprovalSpec {
  id: string
  action: string
  riskSummary?: string
  targetTool?: string
  targetArgsPreview?: unknown
  toolCallID?: string
  timeout?: string
  /** A decision already taken (history). Without one the approval is pending and blocks the story. */
  decision?: { status: 'approved' | 'declined'; actor: string; reason?: string; afterSec: number }
}

export interface ArtifactSpec {
  filename: string
  contentType: string
  body: string
}

export interface Beat {
  /** Gap before this beat, in seconds (scaled to fit the task's run time). */
  gap: number
  events?: EventSpec[]
  logs?: string[]
  approval?: ApprovalSpec
  artifact?: ArtifactSpec
}

export interface Outcome {
  phase: 'Succeeded' | 'Failed' | 'Cancelled'
  message?: string
  /** Plain text/markdown, or a JSON string for the structured result view. */
  result?: string
  pr?: { number: number; status: 'open' | 'merged' | 'closed'; branch: string }
}

export type StoryTiming =
  /** Running at page load: the first `played` beats are history, the rest play over `remainingSec`. */
  | { kind: 'running'; played: number; remainingSec: number }
  /** Pending at page load: starts after `startInSec` and runs for `runSec`. */
  | { kind: 'pending'; startInSec: number; runSec: number }

export interface Story {
  beats: Beat[]
  outcome: Outcome
  /** Path taken when the story's pending approval is declined. */
  declined?: { beats: Beat[]; outcome: Outcome }
  timing?: StoryTiming
}

// ---- builders ---------------------------------------------------------------

const CONTEXT_WINDOWS: Record<string, number> = {
  'claude-sonnet-4-5': 200_000,
  'claude-haiku-4-5': 200_000,
  'gpt-5': 400_000,
  'gpt-4.1-mini': 1_047_576,
  'gpt-4.1': 1_047_576,
}

export interface ModelContext {
  provider: string
  model: string
}

export function modelOf(agent: AgentKey): ModelContext {
  const { provider, model } = AGENTS[agent]
  return { provider, model }
}

let callCounter = 0
const callId = () => `call_${digest(`tool-call-${callCounter++}`, 12)}`

/** A model request: started, then completed (optionally with the model's message). */
export function llm(m: ModelContext, input: number, output: number, opts: { stop?: string; message?: string; secs?: number; cached?: number } = {}): Beat[] {
  const stop = opts.stop ?? (opts.message ? 'end_turn' : 'tool_use')
  const completed: EventSpec = {
    type: 'ModelRequestCompleted',
    provider: m.provider,
    model: m.model,
    stopReason: stop,
    inputTokens: input,
    outputTokens: output,
    cachedInputTokens: opts.cached ?? Math.round(input * 0.6),
    contextWindowUsed: input + output,
    contextWindowSize: CONTEXT_WINDOWS[m.model] ?? 200_000,
    summary: `${m.model} responded (${stop})`,
  }
  return [
    { gap: 1, events: [{ type: 'ModelRequestStarted', provider: m.provider, model: m.model, summary: `Requesting ${m.model}` }] },
    {
      gap: opts.secs ?? 6,
      events: opts.message
        ? [completed, { type: 'ModelMessage', provider: m.provider, model: m.model, summary: firstLine(opts.message), contentText: opts.message }]
        : [completed],
    },
  ]
}

/** A tool call: started with its arguments, then completed (or failed) with its result. */
export function tool(name: string, args: unknown, result: unknown, opts: { summary: string; done?: string; secs?: number; failed?: string; logs?: string[] }): Beat[] {
  const id = callId()
  const finished: EventSpec = opts.failed
    ? { type: 'ToolCallFailed', severity: 'error', toolName: name, toolCallID: id, summary: opts.failed, content: { error: opts.failed } }
    : { type: 'ToolCallCompleted', toolName: name, toolCallID: id, summary: opts.done ?? `${name} completed`, content: { result } }
  return [
    { gap: 1, events: [{ type: 'ToolCallStarted', toolName: name, toolCallID: id, summary: opts.summary, content: { arguments: args } }] },
    { gap: opts.secs ?? 3, events: [finished], logs: opts.logs },
  ]
}

/** A single beat holding one event. */
export const ev = (gap: number, type: string, summary: string, extra: Partial<EventSpec> = {}): Beat => ({
  gap,
  events: [{ type, summary, ...extra }],
})

export const plan = (gap: number, iteration: number, progressPct: number, summary: string, document: string, goalComplete = false): Beat =>
  ev(gap, 'PlanUpdated', summary, { content: { iteration, progressPct, goalComplete }, contentText: document })

/** Clone and check out the task's repository in its workspace. */
export function workspace(repo: RepoKey, ref: string, intent: 'read' | 'write', files: number): Beat[] {
  const sha = digest(`${repo}:${ref}`, 7)
  return [
    ev(1, 'WorkspacePreparationStarted', `Cloning ${REPOS[repo].slug} at ${ref} (${intent} intent)`, { content: { repository: REPOS[repo].slug, ref, intent } }),
    ev(6, 'WorkspacePreparationCompleted', `Workspace ready at ${sha} (${files.toLocaleString('en-US')} files)`, { content: { commit: sha, files } }),
  ]
}

/** An ACP runtime session start and prompt submission. */
export function acpStart(runtime: string, promptBytes: number): Beat[] {
  const pool = currentRuntimePool(runtime).metadata.name
  return [
    ev(2, 'AgentRuntimeStarted', `Session started on RuntimePool ${pool} (orka.harness.v2)`, { content: { runtimePool: pool, contractVersion: 'orka.harness.v2' } }),
    ev(1, 'AgentRuntimeCommandStarted', `Prompt submitted (${promptBytes.toLocaleString('en-US')} bytes)`),
  ]
}

/** A historical approval that was already decided. */
export function decidedApproval(spec: Omit<ApprovalSpec, 'decision'>, decision: NonNullable<ApprovalSpec['decision']>): Beat[] {
  return [
    { gap: 1, approval: { ...spec, decision }, events: [{ type: 'ApprovalRequested', severity: 'warning', summary: `Approval requested: ${spec.action}`, toolName: spec.targetTool, toolCallID: spec.toolCallID, content: { approvalID: spec.id } }] },
    ev(decision.afterSec, decision.status === 'approved' ? 'ApprovalApproved' : 'ApprovalDeclined', `${decision.status === 'approved' ? 'Approved' : 'Declined'} by ${decision.actor}${decision.reason ? `: ${decision.reason}` : ''}`, { content: { approvalID: spec.id } }),
  ]
}

const firstLine = (text: string) => {
  const line = text.split('\n').find((l) => l.trim()) ?? text
  const clean = line.replace(/^#+\s*/, '').replace(/\*\*/g, '')
  return clean.length > 140 ? `${clean.slice(0, 137)}…` : clean
}

const structured = (value: Record<string, unknown>) => JSON.stringify(value)

const workerStarted = (m: ModelContext, maxIterations = 20) =>
  ev(2, 'WorkerStarted', `AI worker started · ${m.provider}/${m.model} · up to ${maxIterations} iterations`)
const resultSubmitted = (bytes: number) => ev(1, 'ResultSubmitted', `Result stored (${(bytes / 1024).toFixed(1)} KB)`)
const workerCompleted = ev(1, 'WorkerCompleted', 'AI worker exited cleanly')
const acpCompleted = (turns: number) => ev(1, 'AgentRuntimeCompleted', `Prompt completed after ${turns} turns`)

// ---- stories ------------------------------------------------------------------

const incident = modelOf('incident')
const coordinator = modelOf('coordinator')
const triage = modelOf('triage')
const reviewer = modelOf('reviewer')
const fixer = modelOf('fixer')
const deps = modelOf('deps')
const docs = modelOf('docs')
const security = modelOf('security')

const LATENCY_RESULT = `## Checkout p99 latency spike — resolved

**Impact:** p99 on \`checkout-service\` rose from ~180 ms to 1.4 s for 19 minutes; error rate peaked at 2.1% (timeouts on \`POST /v1/orders\`).

**Cause:** PR #1477 ("Tune pgx connection pool") lowered \`DB_MAX_CONNS\` from 50 to 20. Under lunchtime load, requests queued for a connection (\`postgresql.connections.waiting\` 0 → 37 at the moment of the spike).

**Mitigation:** Rolled back \`checkout-service\` to v3.18.2 (revision 41) via #deploys. p99 returned to 205 ms within three minutes of the rollout.

**Follow-ups**
1. Re-land #1477 with \`DB_MAX_CONNS=40\` and a load test in the PR pipeline.
2. Alert on \`connections.waiting > 5\` for 2 minutes (currently no alert).
3. PagerDuty INC-518 updated with this summary; Slack thread linked.`

const LATENCY_DECLINED_RESULT = `## Checkout p99 latency spike — mitigation proposed, not applied

The rollback was declined, so nothing was changed in production.

**Most likely cause:** PR #1477 lowered \`DB_MAX_CONNS\` from 50 to 20; waiting connections jumped from 0 to 37 when p99 spiked.

**Options**
1. Roll back to v3.18.2 (fastest, reverts #1475–#1477).
2. Hot-patch \`DB_MAX_CONNS=40\` through the config service (no deploy; restarts pods gradually).

Posted both options to #incidents for the on-call engineer.`

const LATENCY_PLAN_DOC = `# INC-518: checkout-service p99 latency

1. [x] Find the cause: #1477 lowered DB_MAX_CONNS from 50 to 20
2. [x] Roll back checkout-service to v3.18.2 (approved)
3. [x] Confirm p99 recovered: 205 ms`

const LATENCY_DECLINED_PLAN_DOC = `# INC-518: checkout-service p99 latency

1. [x] Find the cause: #1477 lowered DB_MAX_CONNS from 50 to 20
2. [x] Propose a rollback to v3.18.2 (declined)
3. [x] Post mitigation options to #incidents`

const latencyInvestigation: Story = {
  timing: { kind: 'running', played: 22, remainingSec: 75 },
  beats: [
    workerStarted(incident),
    ...llm(incident, 6240, 412),
    ...tool('datadog_query', { query: 'p99:trace.http.request{service:checkout-service,env:prod}', from: 'now-2h' }, { points: 240, before: '182ms', peak: '1412ms', peakAt: '14:04Z' }, { summary: 'datadog_query p99 latency for checkout-service', done: 'p99 rose from 182 ms to 1,412 ms at 14:04' }),
    ...tool('datadog_query', { query: 'sum:trace.http.request.errors{service:checkout-service,env:prod}.as_rate()', from: 'now-2h' }, { before: '0.4%', peak: '2.1%' }, { summary: 'datadog_query error rate for checkout-service', done: 'Error rate 0.4% → 2.1%, mostly 504 on POST /v1/orders' }),
    ...llm(incident, 9820, 388),
    ...tool('github_search', { repo: REPOS.checkout.slug, query: 'is:pr is:merged merged:>=2026-10-15' }, { items: [{ number: 1477, title: 'Tune pgx connection pool' }, { number: 1476, title: 'Log refund reason codes' }, { number: 1475, title: 'Bump otel-go to v1.31' }] }, { summary: 'github_search PRs merged today in checkout-service', done: '3 PRs merged today; #1477 changes DB pool settings' }),
    ...tool('datadog_query', { query: 'avg:postgresql.connections.waiting{db:checkout,env:prod}', from: 'now-2h' }, { before: 0, peak: 37 }, { summary: 'datadog_query waiting Postgres connections', done: 'Waiting connections 0 → 37 at 14:02, matching the spike' }),
    ...llm(incident, 14310, 655, { message: 'Likely cause: #1477 lowered DB_MAX_CONNS from 50 to 20. Requests are queueing for database connections. Proposing a rollback to v3.18.2.' }),
    ...tool('pagerduty_incident', { incident: 'INC-518', action: 'add_note', note: 'Suspected cause: #1477 (pool max 50→20). Proposing rollback.' }, { ok: true }, { summary: 'pagerduty_incident add note to INC-518', done: 'Note added to INC-518' }),
    ...tool('slack_post', { channel: '#incidents', thread: 'INC-518', text: 'p99 spike lines up with #1477 (DB pool 50→20); waiting connections 0→37. Requesting approval to roll back to v3.18.2.' }, { ok: true, ts: '1760536981.004200' }, { summary: 'slack_post to #incidents', done: 'Posted hypothesis to the incident thread' }),
    ...llm(incident, 16020, 301),
    {
      gap: 2,
      events: [
        { type: 'ToolCallStarted', toolName: 'request_approval', toolCallID: 'call_rollback_checkout', summary: 'request_approval: roll back checkout-service to v3.18.2', content: { arguments: { action: 'rollback', deployment: 'checkout-service', toRevision: 41 } } },
        { type: 'ApprovalRequested', severity: 'warning', toolName: 'request_approval', toolCallID: 'call_rollback_checkout', summary: 'Approval requested: roll back checkout-service (prod) to v3.18.2', content: { approvalID: 'apr-rollback-checkout' } },
      ],
      approval: {
        id: 'apr-rollback-checkout',
        action: 'Roll back deployment checkout-service in prod to revision 41 (v3.18.2)',
        riskSummary: 'Production rollback. Reverts #1475–#1477 merged today; expect ~30 s of elevated errors while pods roll.',
        targetTool: 'request_approval',
        targetArgsPreview: { cluster: 'prod-eu-west-1', namespace: 'checkout', deployment: 'checkout-service', toRevision: 41, image: 'ghcr.io/northwind-labs/checkout-service:v3.18.2' },
        toolCallID: 'call_rollback_checkout',
        timeout: '30m',
      },
    },
    ev(1, 'ToolCallCompleted', 'request_approval: approved', { toolName: 'request_approval', toolCallID: 'call_rollback_checkout', content: { result: { approved: true } } }),
    ...tool('slack_post', { channel: '#deploys', text: '@deploy-bot rollback checkout-service prod --to v3.18.2 --reason INC-518' }, { ok: true, ts: '1760537122.001100' }, { summary: 'slack_post rollback command to #deploys', done: 'deploy-bot acknowledged: rolling back checkout-service to v3.18.2' }),
    ...tool('datadog_query', { query: 'p99:trace.http.request{service:checkout-service,env:prod}', from: 'now-10m' }, { now: '205ms', trend: 'recovering' }, { summary: 'datadog_query p99 after rollback', done: 'p99 back to 205 ms three minutes after the rollout', secs: 12 }),
    ...llm(incident, 19800, 1180, { message: LATENCY_RESULT }),
    ...tool('slack_post', { channel: '#incidents', thread: 'INC-518', text: 'Rolled back to v3.18.2; p99 is 205 ms. Summary and follow-ups posted to INC-518.' }, { ok: true }, { summary: 'slack_post resolution to #incidents', done: 'Posted resolution to the incident thread' }),
    // Autonomous coordination: marking the goal complete ends the loop.
    plan(1, 1, 100, 'Incident mitigated: rolled back to v3.18.2', LATENCY_PLAN_DOC, true),
    resultSubmitted(LATENCY_RESULT.length),
    workerCompleted,
  ],
  outcome: { phase: 'Succeeded', result: LATENCY_RESULT },
  declined: {
    beats: [
      ev(2, 'ToolCallCompleted', 'request_approval: declined', { toolName: 'request_approval', toolCallID: 'call_rollback_checkout', content: { result: { approved: false } } }),
      ...llm(incident, 17110, 820, { message: LATENCY_DECLINED_RESULT }),
      ...tool('slack_post', { channel: '#incidents', thread: 'INC-518', text: 'Rollback declined. Options: roll back to v3.18.2, or hot-patch DB_MAX_CONNS=40 via the config service.' }, { ok: true }, { summary: 'slack_post options to #incidents', done: 'Posted mitigation options to the incident thread' }),
      plan(1, 1, 100, 'Rollback declined; mitigation options posted', LATENCY_DECLINED_PLAN_DOC, true),
      resultSubmitted(LATENCY_DECLINED_RESULT.length),
      workerCompleted,
    ],
    outcome: { phase: 'Succeeded', result: LATENCY_DECLINED_RESULT },
  },
}

const CART_DIFF = `diff --git a/internal/pricing/discount.go b/internal/pricing/discount.go
index 4c1d2e7..9a8b3f0 100644
--- a/internal/pricing/discount.go
+++ b/internal/pricing/discount.go
@@ -38,10 +38,13 @@ func ApplyPercentDiscount(items []LineItem, pct decimal.Decimal, cur Currency) Money {
 	total := decimal.Zero
 	for _, item := range items {
 		discounted := item.Price.Mul(decimal.NewFromInt(100).Sub(pct)).Div(decimal.NewFromInt(100))
-		total = total.Add(discounted.Round(2))
+		total = total.Add(discounted)
 	}
-	return Money{Amount: total, Currency: cur}
+	// Round once, at the currency's minor unit. JPY has no minor unit, so
+	// rounding each line to two places and again at checkout drifted by one.
+	return Money{Amount: total.Round(cur.MinorUnits()), Currency: cur}
 }
diff --git a/internal/pricing/discount_test.go b/internal/pricing/discount_test.go
index 1b7e0a2..c3d94f1 100644
--- a/internal/pricing/discount_test.go
+++ b/internal/pricing/discount_test.go
@@ -61,3 +61,18 @@ func TestApplyPercentDiscountUSD(t *testing.T) {
 	}
 }
+
+func TestApplyPercentDiscountZeroDecimalCurrency(t *testing.T) {
+	items := []LineItem{
+		{SKU: "TEA-001", Price: decimal.NewFromInt(1499)},
+		{SKU: "TEA-002", Price: decimal.NewFromInt(2199)},
+		{SKU: "CUP-010", Price: decimal.NewFromInt(899)},
+	}
+	got := ApplyPercentDiscount(items, decimal.NewFromInt(15), JPY)
+	want := decimal.NewFromInt(3907)
+	if !got.Amount.Equal(want) {
+		t.Fatalf("total = %s, want %s", got.Amount, want)
+	}
+}
`

const cartFix: Story = {
  timing: { kind: 'running', played: 14, remainingSec: 330 },
  beats: [
    ...workspace('checkout', 'main', 'write', 1286),
    ...acpStart('codex', 2310),
    ...llm(fixer, 11800, 640),
    ...tool('shell', { command: ['rg', '-n', 'ApplyPercentDiscount', 'internal/'] }, { exitCode: 0, stdout: 'internal/pricing/discount.go:38\ninternal/cart/totals.go:112\ninternal/pricing/discount_test.go:44' }, { summary: 'shell: rg -n ApplyPercentDiscount internal/', done: '3 matches in pricing and cart' }),
    ...tool('shell', { command: ['sed', '-n', '30,60p', 'internal/pricing/discount.go'] }, { exitCode: 0 }, { summary: 'shell: read internal/pricing/discount.go', done: 'Read 31 lines' }),
    ...llm(fixer, 15400, 1220),
    ...tool('apply_patch', { path: 'internal/pricing/discount_test.go' }, { applied: true, hunks: 1 }, { summary: 'apply_patch: add a JPY regression test', done: 'Added TestApplyPercentDiscountZeroDecimalCurrency' }),
    ...tool('shell', { command: ['go', 'test', './internal/pricing/...', '-run', 'ZeroDecimal'] }, { exitCode: 1, stdout: '--- FAIL: TestApplyPercentDiscountZeroDecimalCurrency\n    discount_test.go:74: total = 3908, want 3907' }, { summary: 'shell: go test ./internal/pricing/... -run ZeroDecimal', done: 'Test fails as expected: total = 3908, want 3907', secs: 9 }),
    ...llm(fixer, 18100, 980),
    ...tool('apply_patch', { path: 'internal/pricing/discount.go' }, { applied: true, hunks: 1 }, { summary: 'apply_patch: round once at the currency minor unit', done: 'Updated ApplyPercentDiscount' }),
    ...tool('shell', { command: ['go', 'test', './internal/pricing/...', './internal/cart/...'] }, { exitCode: 0, stdout: 'ok  github.com/northwind-labs/checkout-service/internal/pricing 0.214s\nok  github.com/northwind-labs/checkout-service/internal/cart 0.391s' }, { summary: 'shell: go test ./internal/pricing/... ./internal/cart/...', done: 'All pricing and cart tests pass', secs: 14 }),
    ...tool('shell', { command: ['go', 'vet', './...'] }, { exitCode: 0 }, { summary: 'shell: go vet ./...', done: 'go vet clean', secs: 8 }),
    ...llm(fixer, 21400, 1560, { message: 'Fixed: percentage discounts now round once at the currency minor unit, so zero-decimal currencies like JPY no longer drift by one. Added a regression test; pricing and cart suites pass.' }),
    { gap: 2, events: [{ type: 'ArtifactUploadCompleted', summary: 'Uploaded changes.patch (1.9 KB)', content: { filename: 'changes.patch' } }], artifact: { filename: 'changes.patch', contentType: 'text/x-diff', body: CART_DIFF } },
    acpCompleted(11),
    resultSubmitted(2400),
  ],
  outcome: {
    phase: 'Succeeded',
    pr: { number: 1483, status: 'open', branch: 'orka/fix-jpy-discount-rounding' },
    result: structured({
      summary: 'Percentage discounts rounded each line item to two decimals before summing, then rounded again at checkout. For zero-decimal currencies (JPY) the double rounding added one yen. ApplyPercentDiscount now sums unrounded line totals and rounds once at the currency\'s minor unit.\n\nOpened #1483 with the fix and a regression test (fails before, passes after).',
      files: ['internal/pricing/discount.go', 'internal/pricing/discount_test.go'],
      diff: CART_DIFF,
      pushBranch: 'orka/fix-jpy-discount-rounding',
    }),
  },
}

const RELEASE_PLAN_DOC = `# Release v2.14.0 — web-storefront

1. [x] Collect merged PRs since v2.13.2 (23 PRs)
2. [x] Check release blockers (delegated to issue-triager)
3. [ ] Draft release notes (delegated to docs-writer)
4. [ ] Review notes and open the release PR
5. [ ] Hand off to maya.chen for the tag`

const RELEASE_RESULT = `## web-storefront v2.14.0 is ready to tag

- **23 PRs** merged since v2.13.2 (8 features, 11 fixes, 4 chores)
- **Blockers:** none open — #866 closed by #871, #874 (Safari Apple Pay button) fixed by #879
- **Release notes:** drafted by docs-writer in #881 (\`docs/releases/v2.14.0.md\`, CHANGELOG updated)

### Highlights
- Lazy-loaded product carousel images (-38% LCP on PDP, #873)
- Saved carts sync across devices (#858)
- Checkout address autocomplete for DE/AT/CH (#862)

Next step for @maya.chen: merge #881, then tag \`v2.14.0\` from \`main\`.`

const releasePlan: Story = {
  timing: { kind: 'running', played: 15, remainingSec: 290 },
  beats: [
    workerStarted(coordinator, 30),
    plan(2, 1, 5, 'Planning release v2.14.0', RELEASE_PLAN_DOC.replace('[x] Collect', '[ ] Collect').replace('[x] Check', '[ ] Check')),
    ...llm(coordinator, 8200, 520),
    ...tool('github_search', { repo: REPOS.storefront.slug, query: 'is:pr is:merged base:main merged:>2026-09-24' }, { total: 23 }, { summary: 'github_search merged PRs since v2.13.2', done: '23 PRs merged since v2.13.2' }),
    ...llm(coordinator, 13600, 610),
    plan(1, 1, 25, 'Collected 23 merged PRs', RELEASE_PLAN_DOC.replace('[x] Check', '[ ] Check')),
    ...tool('delegate_task', { agent: AGENTS.triage.name, prompt: 'List open issues labelled release-blocker for milestone v2.14.0 and summarize their status.' }, { task: 'release-v2-14-0-blockers-p3c5v' }, { summary: 'delegate_task to issue-triager: check release blockers', done: 'Created child task release-v2-14-0-blockers-p3c5v' }),
    ...tool('delegate_task', { agent: AGENTS.docs.name, prompt: 'Draft release notes for web-storefront v2.14.0 from the merged PR list.' }, { task: 'release-v2-14-0-notes-h4t6n' }, { summary: 'delegate_task to docs-writer: draft release notes', done: 'Created child task release-v2-14-0-notes-h4t6n' }),
    ev(1, 'ToolCallStarted', 'wait_for_tasks: 2 child tasks', { toolName: 'wait_for_tasks', toolCallID: 'call_wait_release_children', content: { arguments: { tasks: ['release-v2-14-0-blockers-p3c5v', 'release-v2-14-0-notes-h4t6n'] } } }),
    plan(4, 2, 45, 'Blockers checked: none open', RELEASE_PLAN_DOC),
    // Live from here: the notes child finishes, then the coordinator wraps up.
    ev(60, 'ToolCallCompleted', 'wait_for_tasks: both child tasks succeeded', { toolName: 'wait_for_tasks', toolCallID: 'call_wait_release_children', content: { result: { succeeded: 2, failed: 0 } } }),
    plan(2, 3, 80, 'Release notes drafted in #881', RELEASE_PLAN_DOC.replace('3. [ ]', '3. [x]')),
    ...llm(coordinator, 24800, 1450, { message: RELEASE_RESULT }),
    plan(2, 3, 100, 'Release v2.14.0 ready to tag', RELEASE_PLAN_DOC.replace(/\[ \]/g, '[x]'), true),
    resultSubmitted(RELEASE_RESULT.length),
    workerCompleted,
  ],
  outcome: { phase: 'Succeeded', result: RELEASE_RESULT },
}

const NOTES_MD = `# web-storefront v2.14.0

## Features
- Lazy-load product carousel images below the fold (#873)
- Sync saved carts across signed-in devices (#858)
- Address autocomplete at checkout for DE, AT, and CH (#862)
- Show delivery estimates on the cart page (#855)

## Fixes
- Apple Pay button missing on Safari 18 (#879)
- Coupon field accepted whitespace-only codes (#871)
- Breadcrumbs overflowed on small screens (#868)

## Chores
- Upgrade to React 19.2 and Vite 6.4 (#850, #851)
`

const releaseNotes: Story = {
  timing: { kind: 'running', played: 9, remainingSec: 230 },
  beats: [
    ...workspace('storefront', 'main', 'write', 2204),
    ...acpStart('copilot', 3120),
    ...llm(docs, 9600, 540),
    ...tool('bash', { command: 'git log --merges --oneline v2.13.2..main' }, { exitCode: 0, lines: 23 }, { summary: 'bash: git log --merges v2.13.2..main', done: '23 merge commits' }),
    ...tool('view', { path: 'CHANGELOG.md' }, { lines: 412 }, { summary: 'view CHANGELOG.md', done: 'Read 412 lines' }),
    ...llm(docs, 14200, 2210),
    ...tool('str_replace_editor', { command: 'create', path: 'docs/releases/v2.14.0.md' }, { created: true }, { summary: 'create docs/releases/v2.14.0.md', done: 'Wrote release notes (31 lines)' }),
    ...tool('str_replace_editor', { command: 'str_replace', path: 'CHANGELOG.md' }, { replaced: 1 }, { summary: 'edit CHANGELOG.md', done: 'Added the v2.14.0 section' }),
    ...tool('bash', { command: 'npx markdownlint-cli2 docs/releases/v2.14.0.md CHANGELOG.md' }, { exitCode: 0 }, { summary: 'bash: markdownlint release notes', done: 'markdownlint clean', secs: 6 }),
    ...llm(docs, 16900, 480, { message: 'Release notes drafted in docs/releases/v2.14.0.md and the CHANGELOG updated. Opened #881.' }),
    { gap: 1, events: [{ type: 'ArtifactUploadCompleted', summary: 'Uploaded release-notes.md (0.6 KB)', content: { filename: 'release-notes.md' } }], artifact: { filename: 'release-notes.md', contentType: 'text/markdown', body: NOTES_MD } },
    acpCompleted(7),
    resultSubmitted(1300),
  ],
  outcome: {
    phase: 'Succeeded',
    pr: { number: 881, status: 'open', branch: 'orka/release-notes-v2.14.0' },
    result: structured({
      summary: 'Drafted release notes for v2.14.0 from 23 merged PRs (4 features, 3 user-facing fixes, 2 chores worth calling out) and added the CHANGELOG section. Opened #881.',
      files: ['docs/releases/v2.14.0.md', 'CHANGELOG.md'],
      pushBranch: 'orka/release-notes-v2.14.0',
    }),
  },
}

const BLOCKERS_RESULT = `No open release blockers for milestone **v2.14.0**.

| Issue | Status |
| --- | --- |
| #866 Coupon field accepts whitespace | Closed by #871 |
| #874 Apple Pay button missing on Safari 18 | Closed by #879 |

Both fixes are on \`main\` and covered by the release notes.`

const releaseBlockers: Story = {
  beats: [
    workerStarted(triage),
    ...llm(triage, 4100, 220),
    ...tool('github_search', { repo: REPOS.storefront.slug, query: 'is:issue label:release-blocker milestone:v2.14.0' }, { total: 2, open: 0 }, { summary: 'github_search release-blocker issues for v2.14.0', done: '2 issues, both closed' }),
    ...llm(triage, 8700, 1230, { message: BLOCKERS_RESULT }),
    resultSubmitted(BLOCKERS_RESULT.length),
    workerCompleted,
  ],
  outcome: { phase: 'Succeeded', result: BLOCKERS_RESULT },
}

function review(repo: RepoKey, pr: number, files: number, findings: Beat[], result: Record<string, unknown>, timing?: StoryTiming): Story {
  return {
    timing,
    beats: [
      ...workspace(repo, `refs/pull/${pr}/head`, 'read', files),
      ...acpStart('claude', 1840),
      ...llm(reviewer, 12400, 710),
      ...tool('Bash', { command: `git diff --stat origin/main...HEAD` }, { exitCode: 0 }, { summary: 'Bash: git diff --stat origin/main...HEAD', done: 'Read the change summary' }),
      ...findings,
      ...llm(reviewer, 26100, 1640, { message: String(result.summary) }),
      { gap: 1, events: [{ type: 'ArtifactUploadCompleted', summary: 'Uploaded review-comments.json', content: { filename: 'review-comments.json' } }], artifact: { filename: 'review-comments.json', contentType: 'application/json', body: JSON.stringify({ pullRequest: pr, verdict: result.verdict, comments: result.comments ?? [] }, null, 2) } },
      acpCompleted(6),
      resultSubmitted(1800),
    ],
    outcome: { phase: 'Succeeded', result: structured({ verdict: result.verdict, summary: result.summary, feedback: result.feedback, files: result.files }) },
  }
}

const review1482 = review(
  'checkout',
  1482,
  1291,
  [
    ...tool('Read', { file_path: 'internal/payments/capture.go' }, { lines: 184 }, { summary: 'Read internal/payments/capture.go', done: 'Read 184 lines' }),
    ...tool('Read', { file_path: 'migrations/043_payment_idempotency.sql' }, { lines: 12 }, { summary: 'Read migrations/043_payment_idempotency.sql', done: 'Read 12 lines' }),
    ...tool('Grep', { pattern: 'idempotency_key', path: 'internal/' }, { matches: 9 }, { summary: 'Grep idempotency_key in internal/', done: '9 matches' }),
    ...llm(reviewer, 21800, 930),
    ...tool('Bash', { command: 'go test ./internal/payments/... -run Idempotent -count=3' }, { exitCode: 0 }, { summary: 'Bash: go test ./internal/payments/... -run Idempotent', done: 'Tests pass (3 runs)', secs: 12 }),
  ],
  {
    verdict: 'REQUEST_CHANGES',
    summary: 'The idempotency key is stored and checked, but the uniqueness constraint is global rather than per merchant, and a retry that arrives while the first capture is still in flight can double-capture.',
    feedback: '1. migrations/043: make the unique index (merchant_id, idempotency_key); two merchants can legitimately reuse a client-generated key.\n2. capture.go:97: the SELECT-then-INSERT is racy. Insert the key first with ON CONFLICT DO NOTHING and treat a conflict as "in progress" (409) until the first attempt records its result.\n3. Nit: return the original response body for replays, not just the status code.',
    files: ['internal/payments/capture.go', 'migrations/043_payment_idempotency.sql', 'internal/payments/capture_test.go'],
    comments: [{ path: 'internal/payments/capture.go', line: 97, body: 'SELECT-then-INSERT races under concurrent retries.' }],
  },
  { kind: 'pending', startInSec: 15, runSec: 150 },
)

const review1479 = review(
  'checkout',
  1479,
  1288,
  [
    ...tool('Read', { file_path: 'internal/events/outbox.go' }, { lines: 211 }, { summary: 'Read internal/events/outbox.go', done: 'Read 211 lines' }),
    ...tool('Read', { file_path: 'internal/events/relay.go' }, { lines: 156 }, { summary: 'Read internal/events/relay.go', done: 'Read 156 lines' }),
    ...llm(reviewer, 19700, 840),
    ...tool('Bash', { command: 'go test ./internal/events/... -race' }, { exitCode: 0 }, { summary: 'Bash: go test ./internal/events/... -race', done: 'Tests pass with -race', secs: 16 }),
  ],
  {
    verdict: 'APPROVE',
    summary: 'Order events are now written to the outbox in the same transaction as the order, and the relay publishes with FOR UPDATE SKIP LOCKED. Delivery is at-least-once and consumers already dedupe on event ID.',
    feedback: 'Nits (non-blocking):\n- relay.go:88: log the batch size at debug, not info; it runs every 500 ms.\n- Consider an index on outbox(published_at) WHERE published_at IS NULL to keep the poll cheap as the table grows.',
    files: ['internal/events/outbox.go', 'internal/events/relay.go', 'migrations/041_order_outbox.sql'],
  },
)

const review873 = review(
  'storefront',
  873,
  2198,
  [
    ...tool('Read', { file_path: 'src/components/product/Carousel.tsx' }, { lines: 143 }, { summary: 'Read src/components/product/Carousel.tsx', done: 'Read 143 lines' }),
    ...tool('Bash', { command: 'npm run test -- Carousel' }, { exitCode: 0 }, { summary: 'Bash: npm run test -- Carousel', done: '12 tests pass', secs: 9 }),
  ],
  {
    verdict: 'COMMENT',
    summary: 'Lazy-loading below-the-fold slides is a clear win. The first slide should stay eager (and get fetchpriority="high") or LCP regresses on the product page.',
    feedback: '- Carousel.tsx:41: index 0 is lazy too; make it eager.\n- Add width/height to <img> to avoid layout shift while loading.',
    files: ['src/components/product/Carousel.tsx', 'src/components/product/Carousel.test.tsx'],
  },
)

function triageStory(repo: RepoKey, issue: number, query: string, labels: string[], result: string): Story {
  return {
    beats: [
      workerStarted(triage, 10),
      ...llm(triage, 3200, 190),
      ...tool('github_search', { repo: REPOS[repo].slug, query }, { total: 2 }, { summary: `github_search possible duplicates of #${issue}`, done: 'No duplicates; 1 related issue' }),
      ...llm(triage, 5400, 260),
      ...tool('github_search', { repo: REPOS[repo].slug, action: 'add_labels', issue, labels }, { ok: true }, { summary: `github_search label #${issue}: ${labels.join(', ')}`, done: `Labelled #${issue}` }),
      ...llm(triage, 6100, 480, { message: result }),
      resultSubmitted(result.length),
      workerCompleted,
    ],
    outcome: { phase: 'Succeeded', result },
  }
}

const triage3391 = triageStory('inventory', 3391, 'is:issue reservation negative stock', ['bug', 'P1', 'area/reservations'],
  '**#3391 — Stock count goes negative after concurrent reservations**\n\n- Labels: `bug`, `P1`, `area/reservations`\n- Not a duplicate. Related: #3302 (reservation expiry race, fixed in 4.2).\n- Likely cause: `reserve()` reads `available` and decrements in two statements. Suggest `UPDATE ... SET available = available - $1 WHERE available >= $1`.\n- Routed to @dev.patel (owner of reservations).')
const triage3388 = triageStory('inventory', 3388, 'is:issue bulk import SKU', ['enhancement', 'area/api'],
  '**#3388 — Add bulk SKU import endpoint**\n\n- Labels: `enhancement`, `area/api`\n- Duplicate check: none; #3120 (CSV export) is related.\n- Suggested scope: `POST /v2/skus:batchCreate`, max 1,000 rows, async job with status polling.\n- Added to the Q4 API backlog.')
const triage2216 = triageStory('checkout', 2216, 'is:issue apple pay safari', ['bug', 'P2', 'area/payments', 'browser/safari'],
  '**#2216 — Apple Pay button missing on Safari 18**\n\n- Labels: `bug`, `P2`, `area/payments`, `browser/safari`\n- Same root cause as web-storefront#874: `ApplePaySession.canMakePayments()` is now async on Safari 18.\n- Fixed upstream by web-storefront#879; checkout-service only needs the SDK bump to 3.4.1.')

const depsFailed: Story = {
  beats: [
    ...workspace('inventory', 'main', 'write', 734),
    ...acpStart('opencode', 1650),
    ...llm(deps, 8800, 420),
    ...tool('bash', { command: 'uv lock --upgrade' }, { exitCode: 0, upgraded: 17 }, { summary: 'bash: uv lock --upgrade', done: '17 packages upgraded (pydantic 2.9.2 → 3.0.1)', secs: 18 }),
    ...tool('bash', { command: 'uv run pytest -q' }, { exitCode: 1, failed: 41, passed: 612 }, { summary: 'bash: uv run pytest -q', done: '41 failed, 612 passed — pydantic v3 removed Config classes', secs: 40 }),
    ...llm(deps, 23900, 2870),
    ...tool('edit', { path: 'inventory/models/sku.py' }, { applied: true }, { summary: 'edit inventory/models/sku.py: migrate to model_config', done: 'Migrated 6 models' }),
    ...tool('bash', { command: 'uv run pytest -q' }, { exitCode: 1, failed: 29, passed: 624 }, { summary: 'bash: uv run pytest -q', done: '29 failed, 624 passed', secs: 40 }),
    ev(2, 'ContextTruncated', 'Context window 92% full; dropped 14 older tool results', { severity: 'warning', content: { droppedMessages: 14 } }),
    ...llm(deps, 118000, 3410),
    ...tool('edit', { path: 'inventory/api/schemas.py' }, { applied: true }, { summary: 'edit inventory/api/schemas.py: replace validator decorators', done: 'Updated 11 validators' }),
    ...tool('bash', { command: 'uv run pytest -q' }, { exitCode: 1, failed: 18, passed: 635 }, { summary: 'bash: uv run pytest -q', done: '18 failed, 635 passed', secs: 40 }),
    ev(3, 'AgentRuntimeFailed', 'Prompt deadline exceeded after 14m0s', { severity: 'error', content: { reason: 'DeadlineExceeded' } }),
  ],
  outcome: { phase: 'Failed', message: 'agent runtime prompt exceeded the task timeout (15m0s); no changes were published' },
}

const depsSucceeded: Story = {
  beats: [
    ...workspace('inventory', 'main', 'write', 729),
    ...acpStart('opencode', 1650),
    ...llm(deps, 8600, 400),
    ...tool('bash', { command: 'uv lock --upgrade' }, { exitCode: 0, upgraded: 9 }, { summary: 'bash: uv lock --upgrade', done: '9 packages upgraded', secs: 16 }),
    ...tool('bash', { command: 'uv run pytest -q' }, { exitCode: 0, passed: 649 }, { summary: 'bash: uv run pytest -q', done: '649 passed', secs: 38 }),
    ...llm(deps, 11200, 610, { message: 'Upgraded 9 dependencies (fastapi 0.115.2 → 0.115.4, sqlalchemy 2.0.35 → 2.0.36, …). Test suite passes. Opened #3372.' }),
    acpCompleted(4),
    resultSubmitted(900),
  ],
  outcome: {
    phase: 'Succeeded',
    pr: { number: 3372, status: 'merged', branch: 'orka/deps-weekly-20261008' },
    result: structured({
      summary: 'Upgraded 9 dependencies; all 649 tests pass.\n\nfastapi 0.115.2 → 0.115.4\nsqlalchemy 2.0.35 → 2.0.36\nhttpx 0.27.2 → 0.28.0\nstructlog 24.4.0 → 24.5.1\n…and 5 transitive updates.',
      files: ['pyproject.toml', 'uv.lock'],
      pushBranch: 'orka/deps-weekly-20261008',
    }),
  },
}

const RUNBOOK_DIFF = `diff --git a/docs/runbooks/postgres-failover.md b/docs/runbooks/postgres-failover.md
index 2f9e1a0..7c4b8d2 100644
--- a/docs/runbooks/postgres-failover.md
+++ b/docs/runbooks/postgres-failover.md
@@ -12,9 +12,12 @@ Use this when the primary in \`checkout-db\` is unhealthy for more than 5 minutes.
 ## Promote the replica

-1. \`terraform -chdir=envs/prod apply -target=module.checkout_db.aws_db_instance.replica\`
-2. Update \`DATABASE_URL\` in the checkout secret by hand.
+1. \`terraform -chdir=envs/prod apply -var 'checkout_db_failover=true'\`
+   The \`postgres-ha\` module (v2.3) promotes the replica and moves the
+   \`checkout-db.internal\` DNS record; no secret changes are needed.
+2. Watch \`aws_rds_replica_lag\` drop to zero before re-enabling writes.
 3. Page #platform-oncall if promotion takes longer than 10 minutes.
`

const runbookRefresh: Story = {
  beats: [
    ...workspace('infra', 'main', 'write', 512),
    ...acpStart('copilot', 2040),
    ...llm(docs, 10400, 520),
    ...tool('view', { path: 'modules/postgres-ha/README.md' }, { lines: 188 }, { summary: 'view modules/postgres-ha/README.md', done: 'Read 188 lines' }),
    ...tool('view', { path: 'docs/runbooks/postgres-failover.md' }, { lines: 64 }, { summary: 'view docs/runbooks/postgres-failover.md', done: 'Read 64 lines' }),
    ...llm(docs, 14800, 1730),
    ...tool('str_replace_editor', { command: 'str_replace', path: 'docs/runbooks/postgres-failover.md' }, { replaced: 2 }, { summary: 'edit docs/runbooks/postgres-failover.md', done: 'Updated the promotion steps' }),
    ...decidedApproval(
      { id: 'apr-runbook-push', action: 'Push branch orka/runbook-postgres-failover to platform-infra (protected path docs/runbooks/)', riskSummary: 'Writes to a repository path owned by #platform-oncall.', targetTool: 'publish_branch', targetArgsPreview: { repository: REPOS.infra.slug, branch: 'orka/runbook-postgres-failover' } },
      { status: 'approved', actor: 'sam.okafor', reason: 'Matches the module change I merged last week', afterSec: 40 },
    ),
    { gap: 1, events: [{ type: 'ArtifactUploadCompleted', summary: 'Uploaded changes.patch (0.9 KB)', content: { filename: 'changes.patch' } }], artifact: { filename: 'changes.patch', contentType: 'text/x-diff', body: RUNBOOK_DIFF } },
    acpCompleted(5),
    resultSubmitted(1100),
  ],
  outcome: {
    phase: 'Succeeded',
    pr: { number: 214, status: 'open', branch: 'orka/runbook-postgres-failover' },
    result: structured({
      summary: 'The failover runbook still described the manual pre-v2.3 procedure. Updated the promotion steps for the postgres-ha module (single terraform apply, DNS moves automatically) and added a replica-lag check. Opened #214.',
      files: ['docs/runbooks/postgres-failover.md'],
      diff: RUNBOOK_DIFF,
      pushBranch: 'orka/runbook-postgres-failover',
    }),
  },
}

const SCAN_RESULT = `## Security review — checkout-service (payments and refunds)

Reviewed 14 handlers across \`internal/payments\` and \`internal/refunds\` (6,820 lines).

- **Authorization:** refund handlers check merchant ownership, but \`GET /v1/refunds/{id}\` loads the refund before the ownership check and returns 404 vs 403 differently, which leaks refund ID existence.
- **Injection:** no SQL built from strings; all queries use pgx parameters. One \`fmt.Sprintf\` into a log line includes the raw card BIN — should be dropped.
- **Secrets:** webhook signing secret compared with \`==\`; use \`hmac.Equal\`.

Candidate findings were submitted to the Security view for validation and patch proposals.`

const securityScan: Story = {
  beats: [
    ...workspace('checkout', 'main', 'read', 1288),
    ...acpStart('claude', 3260),
    ...llm(security, 18400, 920),
    ...tool('Read', { file_path: 'internal/refunds/handler.go' }, { lines: 342 }, { summary: 'Read internal/refunds/handler.go', done: 'Read 342 lines' }),
    ...tool('Read', { file_path: 'internal/payments/webhook.go' }, { lines: 128 }, { summary: 'Read internal/payments/webhook.go', done: 'Read 128 lines' }),
    ...llm(security, 41200, 1860),
    ...tool('Bash', { command: 'semgrep --config p/golang --json internal/payments internal/refunds' }, { exitCode: 0, results: 5 }, { summary: 'Bash: semgrep p/golang on payments and refunds', done: '5 raw results (2 after triage)', secs: 30 }),
    ...llm(security, 66800, 3120),
    ...tool('Read', { file_path: 'internal/refunds/store.go' }, { lines: 201 }, { summary: 'Read internal/refunds/store.go', done: 'Read 201 lines' }),
    ...llm(security, 85600, 8100, { message: SCAN_RESULT }),
    { gap: 1, events: [{ type: 'ArtifactUploadCompleted', summary: 'Uploaded semgrep.sarif.json (7.4 KB)', content: { filename: 'semgrep.sarif.json' } }], artifact: { filename: 'semgrep.sarif.json', contentType: 'application/json', body: JSON.stringify({ version: '2.1.0', runs: [{ tool: { driver: { name: 'semgrep', version: '1.95.0' } }, results: [{ ruleId: 'go.lang.security.audit.crypto.timing-attack', level: 'warning', locations: [{ physicalLocation: { artifactLocation: { uri: 'internal/payments/webhook.go' }, region: { startLine: 54 } } }] }] }] }, null, 2) } },
    acpCompleted(6),
    resultSubmitted(SCAN_RESULT.length),
  ],
  outcome: { phase: 'Succeeded', result: SCAN_RESULT },
}

const FLAKY_DIFF = `diff --git a/tests/test_reservations.py b/tests/test_reservations.py
index 0d4a7e1..5b92c3f 100644
--- a/tests/test_reservations.py
+++ b/tests/test_reservations.py
@@ -88,9 +88,10 @@ async def test_reservation_expiry(db, clock):
     reservation = await reserve(db, sku="SKU-1001", qty=2, ttl=timedelta(seconds=30))
-    await asyncio.sleep(31)
-    await expire_reservations(db)
+    clock.advance(timedelta(seconds=31))
+    await expire_reservations(db, now=clock.now())
     stock = await get_stock(db, "SKU-1001")
     assert stock.available == 10
`

const flakyFix: Story = {
  beats: [
    ...workspace('inventory', 'main', 'write', 726),
    ...acpStart('codex', 1980),
    ...llm(fixer, 9800, 610),
    ...tool('shell', { command: ['uv', 'run', 'pytest', 'tests/test_reservations.py::test_reservation_expiry', '--count=50'] }, { exitCode: 1, failed: 3, passed: 47 }, { summary: 'shell: pytest test_reservation_expiry --count=50', done: 'Reproduced: 3 of 50 runs fail', secs: 60 }),
    ...llm(fixer, 15600, 1240),
    ...tool('apply_patch', { path: 'tests/test_reservations.py' }, { applied: true }, { summary: 'apply_patch: use the injected clock instead of sleep', done: 'Replaced asyncio.sleep with clock.advance' }),
    ...tool('shell', { command: ['uv', 'run', 'pytest', 'tests/test_reservations.py::test_reservation_expiry', '--count=200'] }, { exitCode: 0, passed: 200 }, { summary: 'shell: pytest test_reservation_expiry --count=200', done: '200 of 200 runs pass', secs: 70 }),
    ...llm(fixer, 18900, 980, { message: 'The test slept 31 s of wall time while expiry used the injected clock, so CI scheduling jitter decided the outcome. It now advances the fake clock; 200/200 runs pass. Opened #3384.' }),
    { gap: 1, events: [{ type: 'ArtifactUploadCompleted', summary: 'Uploaded changes.patch (0.7 KB)', content: { filename: 'changes.patch' } }], artifact: { filename: 'changes.patch', contentType: 'text/x-diff', body: FLAKY_DIFF } },
    acpCompleted(8),
    resultSubmitted(1200),
  ],
  outcome: {
    phase: 'Succeeded',
    pr: { number: 3384, status: 'merged', branch: 'orka/fix-flaky-reservation-expiry' },
    result: structured({
      summary: 'test_reservation_expiry mixed wall-clock sleep with the injected test clock. Under CI load the expiry sweep sometimes ran before 31 s had elapsed on the fake clock. The test now advances the fake clock directly: 200/200 runs pass (was 47/50).',
      files: ['tests/test_reservations.py'],
      diff: FLAKY_DIFF,
      pushBranch: 'orka/fix-flaky-reservation-expiry',
    }),
  },
}

const TERRAFORM_LOGS = [
  'Initializing the backend...',
  'Successfully configured the backend "s3"! Terraform will automatically use this backend unless the backend configuration changes.',
  'Initializing modules...',
  '- checkout_db in modules/postgres-ha',
  '- edge in modules/cloudfront',
  'Initializing provider plugins...',
  '- Reusing previous version of hashicorp/aws from the dependency lock file',
  '- Using previously-installed hashicorp/aws v5.72.1',
  'Terraform has been successfully initialized!',
  'module.checkout_db.aws_db_instance.primary: Refreshing state... [id=checkout-db-prod]',
  'module.checkout_db.aws_db_instance.replica: Refreshing state... [id=checkout-db-prod-replica]',
  'module.edge.aws_cloudfront_distribution.storefront: Refreshing state... [id=E2Q8XK41M0ZP7A]',
  'aws_eks_cluster.prod: Refreshing state... [id=prod-eu-west-1]',
  '',
  'No changes. Your infrastructure matches the configuration.',
  '',
  'Terraform has compared your real infrastructure against your configuration and found no differences, so no changes are needed.',
]

const terraformDrift: Story = {
  beats: [
    ev(1, 'WorkerStarted', 'Container terraform started (hashicorp/terraform:1.9.8)'),
    { gap: 30, logs: TERRAFORM_LOGS.slice(0, 9) },
    { gap: 60, logs: TERRAFORM_LOGS.slice(9) },
    ev(2, 'WorkerCompleted', 'Container terraform exited with code 0'),
    resultSubmitted(140),
  ],
  outcome: { phase: 'Succeeded', result: 'terraform plan exited 0: no drift in the production workspace (4 resources refreshed).' },
}

const migrateFailed: Story = {
  beats: [
    ev(1, 'WorkerStarted', 'Container migrate started (ghcr.io/northwind-labs/checkout-migrate:v42)'),
    {
      gap: 20,
      logs: [
        'level=info msg="connecting to database" host=checkout-db.staging.internal db=checkout',
        'level=info msg="current schema version" version=41',
        'level=info msg="applying migration" version=42 name=add_order_idempotency_key',
        'level=error msg="migration failed" version=42 error="ERROR: column \\"idempotency_key\\" of relation \\"orders\\" already exists (SQLSTATE 42701)"',
        'level=info msg="transaction rolled back" version=42',
        'level=error msg="exiting" code=1',
      ],
    },
    ev(2, 'WorkerFailed', 'Container migrate exited with code 1', { severity: 'error', content: { exitCode: 1 } }),
  ],
  outcome: { phase: 'Failed', message: 'Job failed: container migrate exited with code 1 (column "idempotency_key" of relation "orders" already exists)' },
}

const a11yCancelled: Story = {
  beats: [
    ...workspace('storefront', 'main', 'read', 2201),
    ...acpStart('claude', 1420),
    ...llm(reviewer, 11200, 690),
    ...tool('Bash', { command: 'npx playwright test e2e/a11y/checkout.spec.ts' }, { exitCode: 1, violations: 14 }, { summary: 'Bash: run axe checks on checkout pages', done: '14 WCAG 2.2 AA violations on 5 pages', secs: 50 }),
    ...llm(reviewer, 20600, 1410),
    ...decidedApproval(
      { id: 'apr-a11y-issues', action: 'File 14 GitHub issues in northwind-labs/web-storefront', riskSummary: 'Creates 14 public issues; several may duplicate existing a11y epics.', targetTool: 'github_create_issues', targetArgsPreview: { repository: REPOS.storefront.slug, count: 14, labels: ['a11y'] } },
      { status: 'declined', actor: 'maya.chen', reason: 'Group them under the existing a11y epic instead', afterSec: 60 },
    ),
    ev(5, 'AgentRuntimeCancelled', 'Cancelled by maya.chen', { severity: 'warning' }),
  ],
  outcome: { phase: 'Cancelled', message: 'Cancelled by maya.chen' },
}

const INC512_RESULT = `# INC-512 — inventory sync backlog (post-incident summary)

**Duration:** 47 minutes · **Severity:** SEV-2 · **Customer impact:** stock levels on the storefront lagged up to 40 minutes; 212 orders were accepted for items that had sold out.

## Timeline (UTC)
- 09:12 — Kafka consumer lag on \`inventory.stock-events\` starts climbing
- 09:31 — PagerDuty alert; on-call (dev.patel) acknowledges
- 09:44 — Root cause found: a schema change added a nullable field and the consumer's strict decoder rejected every message, retrying forever
- 09:52 — Consumer redeployed with lenient decoding; backlog drained by 09:59

## Root cause
The producer and consumer ship from different repositories and the schema registry compatibility mode was \`NONE\` for this subject.

## Action items
1. Set registry compatibility to \`BACKWARD\` for all inventory subjects (owner: dev.patel)
2. Dead-letter undecodable messages after 3 attempts instead of retrying forever (owner: lena.fischer)
3. Alert on consumer lag > 2 minutes, not 15 (owner: sam.okafor)`

const inc512: Story = {
  beats: [
    workerStarted(incident),
    ...llm(incident, 9200, 410),
    ...tool('pagerduty_incident', { incident: 'INC-512', action: 'get_timeline' }, { entries: 23 }, { summary: 'pagerduty_incident timeline for INC-512', done: '23 timeline entries' }),
    ...tool('web_fetch', { url: 'https://northwind.slack.example/archives/C04INCIDENTS/p1760432110' }, { messages: 86 }, { summary: 'web_fetch the #incidents thread', done: 'Fetched 86 messages' }),
    ...llm(incident, 31000, 3900, { message: INC512_RESULT }),
    ...tool('pagerduty_incident', { incident: 'INC-512', action: 'add_note', note: 'Post-incident summary drafted by Orka.' }, { ok: true }, { summary: 'pagerduty_incident add summary to INC-512', done: 'Summary attached to INC-512' }),
    plan(1, 1, 100, 'Post-incident summary attached to INC-512', '# INC-512 post-incident summary\n\n1. [x] Read the PagerDuty timeline and #incidents thread\n2. [x] Draft the summary\n3. [x] Attach it to INC-512', true),
    resultSubmitted(INC512_RESULT.length),
    workerCompleted,
  ],
  outcome: { phase: 'Succeeded', result: INC512_RESULT },
}

const OUTBOX_RESULT = `The outbox relay gives **at-least-once** delivery by never publishing anything that isn't committed, and never forgetting anything that is.

1. **Same transaction.** \`CreateOrder\` inserts the order row and an \`outbox\` row in one Postgres transaction (\`internal/events/outbox.go\`). Either both commit or neither does, so there is no "order saved but event lost" window.
2. **Relay loop.** \`relay.go\` polls every 500 ms with \`SELECT ... FOR UPDATE SKIP LOCKED LIMIT 100\`, so multiple relay replicas share the work without double-claiming a row.
3. **Publish, then mark.** Each event is published to Kafka with \`acks=all\`, and only then is \`published_at\` set. If the relay crashes between the two, the row is picked up again and published a second time — that's the "at least once".
4. **Consumers dedupe** on the event ID (a UUIDv7 generated with the outbox row), which turns at-least-once delivery into effectively-once processing.

The one thing it does **not** guarantee is global ordering: events for the same order are ordered (same partition key), but different orders can interleave.`

const outboxExplain: Story = {
  beats: [
    workerStarted(coordinator),
    ...llm(coordinator, 5200, 260),
    ...tool('github_search', { repo: REPOS.checkout.slug, query: 'path:internal/events outbox relay' }, { files: ['internal/events/outbox.go', 'internal/events/relay.go'] }, { summary: 'github_search outbox code in checkout-service', done: 'Found outbox.go and relay.go' }),
    ...llm(coordinator, 16300, 1640, { message: OUTBOX_RESULT }),
    // release-captain is an autonomous coordinator: marking the goal complete ends the loop.
    plan(1, 1, 100, 'Explained the outbox relay', '# Explain the outbox relay\n\n1. [x] Read internal/events/outbox.go and relay.go\n2. [x] Answer in chat', true),
    resultSubmitted(OUTBOX_RESULT.length),
    workerCompleted,
  ],
  outcome: { phase: 'Succeeded', result: OUTBOX_RESULT },
}

/** Stories for the world.ts task catalog, by task name. */
export const STORIES: Record<string, Story> = {
  'checkout-latency-investigation-7k2qp': latencyInvestigation,
  'fix-cart-rounding-bug-x9m4d': cartFix,
  'release-v2-14-0-plan-r2w8z': releasePlan,
  'release-v2-14-0-notes-h4t6n': releaseNotes,
  'release-v2-14-0-blockers-p3c5v': releaseBlockers,
  'review-pr-1482-q8m4d': review1482,
  'review-pr-1479-b7n2k': review1479,
  'review-pr-873-f5j9w': review873,
  'triage-issue-3391-m2x7c': triage3391,
  'triage-issue-3388-k4v1s': triage3388,
  'triage-issue-2216-w9d3e': triage2216,
  'deps-weekly-bump-20261015': depsFailed,
  'deps-weekly-bump-20261008': depsSucceeded,
  'docs-runbook-refresh-j6p2r': runbookRefresh,
  'security-scan-checkout-t3y8u': securityScan,
  'fix-flaky-inventory-test-n8g5h': flakyFix,
  'terraform-drift-check-u5r1a': terraformDrift,
  'db-migrate-orders-v42-e2l9o': migrateFailed,
  'storefront-a11y-audit-c1z6b': a11yCancelled,
  'incident-summary-inc-512-v7s4q': inc512,
  'chat-explain-outbox-d4k8m': outboxExplain,
}

/** Container spec for the catalog's container tasks. */
export const CONTAINERS: Record<string, { image: string; command: string[]; args?: string[] }> = {
  'terraform-drift-check-u5r1a': { image: 'hashicorp/terraform:1.9.8', command: ['terraform', 'plan'], args: ['-detailed-exitcode', '-input=false', '-lock=false'] },
  'db-migrate-orders-v42-e2l9o': { image: 'ghcr.io/northwind-labs/checkout-migrate:v42', command: ['migrate', 'up'], args: ['--env', 'staging', '--to', '42'] },
}

/** Sessions the catalog's tasks append to, beyond the ones world.ts names on the seeds. */
export const EXTRA_SESSION_LINKS: Record<string, string> = {
  'review-pr-1479-b7n2k': 'github-pr-1479-a3e8d2',
  'release-v2-14-0-plan-r2w8z': 'chat-release-v2-14-4d77aa',
}

// ---- tasks created while the demo is open -------------------------------------

/**
 * A short story for a task created during the demo (form, chat, fork). No
 * model is called, so the result says so plainly.
 */
export function liveStory(type: 'ai' | 'agent' | 'container', opts: { agent?: AgentKey; prompt: string; image?: string; repo?: RepoKey }): Story {
  if (type === 'container') {
    const image = opts.image || 'busybox:1.36'
    return {
      beats: [
        ev(3, 'WorkerStarted', `Container started (${image})`),
        { gap: 6, logs: [`Pulled image "${image}"`, 'Started container main'] },
        { gap: 10, logs: ['(sample output) container ran in the demo; no real workload was started'] },
        ev(4, 'WorkerCompleted', 'Container exited with code 0'),
        resultSubmitted(120),
      ],
      outcome: { phase: 'Succeeded', result: 'Container exited with code 0.\n\n(This is the Orka demo: no container actually ran.)' },
    }
  }
  const agent = opts.agent ?? 'coordinator'
  const m = modelOf(agent)
  const note = demoResult(AGENTS[agent].name, opts.prompt)
  if (type === 'ai') {
    return {
      beats: [
        workerStarted(m),
        ...llm(m, 3800, 240, { secs: 5 }),
        ...tool('web_search', { query: opts.prompt.slice(0, 80) }, { results: 5 }, { summary: 'web_search for context', done: '5 results', secs: 4 }),
        ...llm(m, 7600, 720, { message: note, secs: 8 }),
        // An autonomous coordinator's Task ends only once its plan marks the goal complete.
        ...(isAutonomousAgent(agent) ? [plan(1, 1, 100, 'Answered the request', '# Request\n\n1. [x] Answer it', true)] : []),
        resultSubmitted(note.length),
        workerCompleted,
      ],
      outcome: { phase: 'Succeeded', result: note },
    }
  }
  const runtime = 'runtime' in AGENTS[agent] ? (AGENTS[agent] as { runtime: string }).runtime : 'claude'
  return {
    beats: [
      ...(opts.repo ? workspace(opts.repo, 'main', 'read', 1200) : []),
      ...acpStart(runtime, opts.prompt.length + 900),
      ...llm(m, 6400, 380, { secs: 5 }),
      ...tool(runtime === 'claude' ? 'Read' : 'bash', { path: 'README.md' }, { lines: 120 }, { summary: 'Read README.md', done: 'Read 120 lines', secs: 3 }),
      ...llm(m, 9100, 640, { message: note, secs: 8 }),
      acpCompleted(3),
      resultSubmitted(note.length),
    ],
    outcome: { phase: 'Succeeded', result: structured({ summary: note, files: [] }) },
  }
}

const demoResult = (agentName: string, prompt: string) =>
  `This is the Orka demo, so no model was called and nothing ran in a cluster.\n\nIn a live installation, ${agentName} would work on:\n\n> ${prompt.trim().split('\n').join('\n> ')}\n\nand its answer would appear here. The Runtime, Timeline, and Trace tabs show the shape of a real run.`

/** Recurring background work, so the dashboard keeps moving while the demo is open. */
export interface AmbientTemplate {
  prefix: string
  agent?: AgentKey
  type: 'ai' | 'agent' | 'container'
  repo?: RepoKey
  prompt: (n: number) => string
  story: (n: number) => Story
  createdBy: (typeof PEOPLE)[number] | 'gateway' | 'monitor' | 'schedule'
}

export const AMBIENT: AmbientTemplate[] = [
  {
    prefix: 'triage-issue',
    agent: 'triage',
    type: 'ai',
    repo: 'inventory',
    createdBy: 'monitor',
    prompt: (n) => `Triage issue #${3395 + n} "SKU search returns archived products".`,
    story: (n) => triageStory('inventory', 3395 + n, 'is:issue archived sku search', ['bug', 'P3', 'area/search'],
      `**#${3395 + n} — SKU search returns archived products**\n\n- Labels: \`bug\`, \`P3\`, \`area/search\`\n- Not a duplicate.\n- Cause: the search index sync does not delete documents when a SKU is archived.\n- Routed to @lena.fischer (search owner).`),
  },
  {
    prefix: 'review-pr',
    agent: 'reviewer',
    type: 'agent',
    repo: 'checkout',
    createdBy: 'gateway',
    prompt: (n) => `Review PR #${1484 + n} "Bump stripe-go to v82".`,
    story: (n) => review('checkout', 1484 + n, 1291,
      [...tool('Bash', { command: 'go build ./... && go test ./internal/payments/...' }, { exitCode: 0 }, { summary: 'Bash: go build and test payments', done: 'Build and tests pass', secs: 14 })],
      { verdict: 'APPROVE', summary: 'Mechanical upgrade; the two renamed fields are updated and payment tests pass.', feedback: 'No blocking comments.', files: ['go.mod', 'go.sum', 'internal/payments/stripe.go'] }),
  },
  {
    prefix: 'terraform-drift-check',
    type: 'container',
    repo: 'infra',
    createdBy: 'schedule',
    prompt: () => 'terraform plan -detailed-exitcode against the production workspace.',
    story: () => terraformDrift,
  },
  {
    prefix: 'triage-issue',
    agent: 'triage',
    type: 'ai',
    repo: 'checkout',
    createdBy: 'monitor',
    prompt: (n) => `Triage issue #${2219 + n} "Gift card balance shown in the wrong currency".`,
    story: (n) => triageStory('checkout', 2219 + n, 'is:issue gift card currency', ['bug', 'P2', 'area/payments'],
      `**#${2219 + n} — Gift card balance shown in the wrong currency**\n\n- Labels: \`bug\`, \`P2\`, \`area/payments\`\n- Duplicate check: none.\n- The balance endpoint returns minor units without a currency; the UI assumes the store currency.\n- Routed to @jordan.reyes.`),
  },
]

