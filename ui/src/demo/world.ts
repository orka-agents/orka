// The fictional organization behind the demo data. Every domain's fixtures
// draw names from here so cross-links (task -> agent, usage -> task, monitor
// -> repository, gateway -> session) resolve to objects that exist.
//
// Northwind Labs is made up: a mid-size company running Orka for its platform
// team. Repositories, people, and URLs below are fake.

export const ORG = 'northwind-labs'

export const REPOS = {
  checkout: { slug: `${ORG}/checkout-service`, url: `https://github.com/${ORG}/checkout-service`, language: 'Go' },
  inventory: { slug: `${ORG}/inventory-api`, url: `https://github.com/${ORG}/inventory-api`, language: 'Python' },
  storefront: { slug: `${ORG}/web-storefront`, url: `https://github.com/${ORG}/web-storefront`, language: 'TypeScript' },
  infra: { slug: `${ORG}/platform-infra`, url: `https://github.com/${ORG}/platform-infra`, language: 'HCL' },
} as const

export type RepoKey = keyof typeof REPOS

export const PEOPLE = ['maya.chen', 'dev.patel', 'sam.okafor', 'lena.fischer', 'jordan.reyes'] as const

/** Provider CRs (namespace default). Models are served through each provider. */
export const PROVIDERS = {
  anthropic: { name: 'anthropic', type: 'anthropic', models: ['claude-sonnet-4-5', 'claude-haiku-4-5'] },
  openai: { name: 'openai', type: 'openai', models: ['gpt-5', 'gpt-4.1-mini'] },
  azure: { name: 'azure-openai', type: 'openai', models: ['gpt-4.1'] },
} as const

/** Built-in ACP agent runtimes. */
export const RUNTIMES = ['claude', 'codex', 'copilot', 'opencode'] as const

/**
 * Agent CRs. `kind: 'ai'` agents run Orka's own tool loop against a provider;
 * `kind: 'agent'` agents run a coding-agent runtime over ACP in a workspace.
 */
export const AGENTS = {
  triage: { name: 'issue-triager', kind: 'ai', provider: 'openai', model: 'gpt-4.1-mini', description: 'Labels, deduplicates, and routes new GitHub issues.' },
  reviewer: { name: 'code-reviewer', kind: 'agent', runtime: 'claude', provider: 'anthropic', model: 'claude-sonnet-4-5', description: 'Reviews pull requests and leaves inline comments.' },
  coordinator: { name: 'release-captain', kind: 'ai', provider: 'anthropic', model: 'claude-sonnet-4-5', description: 'Plans releases and delegates work to other agents.' },
  incident: { name: 'incident-responder', kind: 'ai', provider: 'anthropic', model: 'claude-sonnet-4-5', description: 'Investigates alerts, correlates deploys, and drafts incident notes.' },
  fixer: { name: 'bug-fixer', kind: 'agent', runtime: 'codex', provider: 'openai', model: 'gpt-5', description: 'Reproduces bugs and opens fix pull requests.' },
  deps: { name: 'dependency-bot', kind: 'agent', runtime: 'opencode', provider: 'openai', model: 'gpt-4.1-mini', description: 'Bumps dependencies and verifies the build.' },
  docs: { name: 'docs-writer', kind: 'agent', runtime: 'copilot', provider: 'azure-openai', model: 'gpt-4.1', description: 'Keeps READMEs and runbooks current with the code.' },
  security: { name: 'security-auditor', kind: 'agent', runtime: 'claude', provider: 'anthropic', model: 'claude-sonnet-4-5', description: 'Reviews code slices for vulnerabilities and validates findings.' },
} as const

export type AgentKey = keyof typeof AGENTS

/** Built-in and custom tools registered in the namespace. */
export const TOOLS = ['web_search', 'web_fetch', 'code_exec', 'file_read', 'file_write', 'request_approval', 'github_search', 'pagerduty_incident', 'datadog_query', 'slack_post'] as const

export type DemoTaskPhase = 'Pending' | 'Running' | 'Succeeded' | 'Failed' | 'Cancelled' | 'Scheduled'

export interface DemoTaskSeed {
  name: string
  agent?: AgentKey
  /** container tasks have no agent */
  type: 'ai' | 'agent' | 'container'
  phase: DemoTaskPhase
  repo?: RepoKey
  /** minutes before page load the task was created */
  createdAgo: number
  /** run time in minutes (for terminal tasks) */
  durationMin?: number
  prompt: string
  inputTokens?: number
  outputTokens?: number
  /** session the task belongs to, if any */
  session?: string
  /** parent coordinator or scheduled task */
  parent?: string
  createdBy?: (typeof PEOPLE)[number] | 'gateway' | 'monitor' | 'schedule' | 'chat'
}

/**
 * The task catalog. Task, usage, board, monitor, and gateway fixtures
 * all reference these names.
 */
export const TASKS: DemoTaskSeed[] = [
  { name: 'checkout-latency-investigation-7k2qp', agent: 'incident', type: 'ai', phase: 'Running', createdAgo: 4, prompt: 'p99 latency on checkout-service jumped from 180ms to 1.4s at 14:02 UTC. Correlate with recent deploys and database metrics, and propose a mitigation.', inputTokens: 48210, outputTokens: 3120, session: 'slack-incidents-c7f3e1', createdBy: 'gateway' },
  { name: 'fix-cart-rounding-bug-x9m4d', agent: 'fixer', type: 'agent', phase: 'Running', repo: 'checkout', createdAgo: 11, prompt: 'Issue #2214: cart totals are off by one cent when a percentage discount applies to items priced in JPY. Reproduce with a failing test, fix the rounding, and open a PR.', inputTokens: 126400, outputTokens: 9850, createdBy: 'monitor' },
  { name: 'release-v2-14-0-plan-r2w8z', agent: 'coordinator', type: 'ai', phase: 'Running', repo: 'storefront', createdAgo: 26, prompt: 'Prepare release v2.14.0 of web-storefront: collect merged PRs since v2.13.2, check for blocking issues, draft release notes, and update the docs.', inputTokens: 64300, outputTokens: 7400, createdBy: 'maya.chen' },
  { name: 'release-v2-14-0-notes-h4t6n', agent: 'docs', type: 'agent', phase: 'Running', repo: 'storefront', createdAgo: 19, prompt: 'Draft release notes for web-storefront v2.14.0 from the merged PR list attached by release-captain.', inputTokens: 38200, outputTokens: 4100, parent: 'release-v2-14-0-plan-r2w8z', createdBy: 'chat' },
  { name: 'release-v2-14-0-blockers-p3c5v', agent: 'triage', type: 'ai', phase: 'Succeeded', repo: 'storefront', createdAgo: 22, durationMin: 2, prompt: 'List open issues labelled release-blocker for milestone v2.14.0 and summarize their status.', inputTokens: 12800, outputTokens: 1450, parent: 'release-v2-14-0-plan-r2w8z', createdBy: 'chat' },
  { name: 'review-pr-1482-q8m4d', agent: 'reviewer', type: 'agent', phase: 'Pending', repo: 'checkout', createdAgo: 1, prompt: 'Review PR #1482 "Add idempotency keys to payment capture". Focus on retry safety and database transactions.', createdBy: 'gateway' },
  { name: 'review-pr-1479-b7n2k', agent: 'reviewer', type: 'agent', phase: 'Succeeded', repo: 'checkout', createdAgo: 52, durationMin: 6, prompt: 'Review PR #1479 "Migrate order events to the outbox pattern".', inputTokens: 91200, outputTokens: 6230, createdBy: 'gateway' },
  { name: 'review-pr-873-f5j9w', agent: 'reviewer', type: 'agent', phase: 'Succeeded', repo: 'storefront', createdAgo: 95, durationMin: 4, prompt: 'Review PR #873 "Lazy-load product carousel images".', inputTokens: 54100, outputTokens: 3980, createdBy: 'gateway' },
  { name: 'triage-issue-3391-m2x7c', agent: 'triage', type: 'ai', phase: 'Succeeded', repo: 'inventory', createdAgo: 38, durationMin: 1, prompt: 'Triage issue #3391 "Stock count goes negative after concurrent reservations".', inputTokens: 8400, outputTokens: 960, createdBy: 'monitor' },
  { name: 'triage-issue-3388-k4v1s', agent: 'triage', type: 'ai', phase: 'Succeeded', repo: 'inventory', createdAgo: 140, durationMin: 1, prompt: 'Triage issue #3388 "Add bulk SKU import endpoint".', inputTokens: 7100, outputTokens: 820, createdBy: 'monitor' },
  { name: 'triage-issue-2216-w9d3e', agent: 'triage', type: 'ai', phase: 'Succeeded', repo: 'checkout', createdAgo: 210, durationMin: 1, prompt: 'Triage issue #2216 "Apple Pay button missing on Safari 18".', inputTokens: 6900, outputTokens: 740, createdBy: 'monitor' },
  { name: 'deps-weekly-bump-20261015', agent: 'deps', type: 'agent', phase: 'Failed', repo: 'inventory', createdAgo: 180, durationMin: 14, prompt: 'Bump Python dependencies to their latest compatible versions, run the test suite, and open one PR.', inputTokens: 143000, outputTokens: 11200, parent: 'deps-weekly-bump', createdBy: 'schedule' },
  { name: 'deps-weekly-bump-20261008', agent: 'deps', type: 'agent', phase: 'Succeeded', repo: 'inventory', createdAgo: 10260, durationMin: 11, prompt: 'Bump Python dependencies to their latest compatible versions, run the test suite, and open one PR.', inputTokens: 118500, outputTokens: 9400, parent: 'deps-weekly-bump', createdBy: 'schedule' },
  { name: 'deps-weekly-bump', agent: 'deps', type: 'agent', phase: 'Scheduled', repo: 'inventory', createdAgo: 43200, prompt: 'Bump Python dependencies to their latest compatible versions, run the test suite, and open one PR.', createdBy: 'lena.fischer' },
  { name: 'docs-runbook-refresh-j6p2r', agent: 'docs', type: 'agent', phase: 'Succeeded', repo: 'infra', createdAgo: 320, durationMin: 9, prompt: 'Update the on-call runbook for the Postgres failover procedure to match the new Terraform module.', inputTokens: 72400, outputTokens: 8800, createdBy: 'sam.okafor' },
  { name: 'security-scan-checkout-t3y8u', agent: 'security', type: 'agent', phase: 'Succeeded', repo: 'checkout', createdAgo: 400, durationMin: 18, prompt: 'Scan checkout-service for authorization and injection issues in the payment and refund handlers.', inputTokens: 212000, outputTokens: 15400, createdBy: 'monitor' },
  { name: 'fix-flaky-inventory-test-n8g5h', agent: 'fixer', type: 'agent', phase: 'Succeeded', repo: 'inventory', createdAgo: 460, durationMin: 12, prompt: 'test_reservation_expiry is flaky in CI (fails ~1 in 20). Find the race and fix it.', inputTokens: 98700, outputTokens: 7650, createdBy: 'dev.patel' },
  { name: 'terraform-drift-check-u5r1a', type: 'container', phase: 'Succeeded', repo: 'infra', createdAgo: 75, durationMin: 3, prompt: 'terraform plan -detailed-exitcode against the production workspace.', createdBy: 'schedule' },
  { name: 'db-migrate-orders-v42-e2l9o', type: 'container', phase: 'Failed', repo: 'checkout', createdAgo: 250, durationMin: 2, prompt: 'Run migration 042_add_order_idempotency_key against staging.', createdBy: 'jordan.reyes' },
  { name: 'storefront-a11y-audit-c1z6b', agent: 'reviewer', type: 'agent', phase: 'Cancelled', repo: 'storefront', createdAgo: 600, durationMin: 5, prompt: 'Audit the checkout flow pages for WCAG 2.2 AA issues and file one issue per finding.', inputTokens: 31800, outputTokens: 2100, createdBy: 'maya.chen' },
  { name: 'incident-summary-inc-512-v7s4q', agent: 'incident', type: 'ai', phase: 'Succeeded', createdAgo: 1500, durationMin: 3, prompt: 'Write the post-incident summary for INC-512 (inventory sync backlog) from the Slack thread and PagerDuty timeline.', inputTokens: 40200, outputTokens: 5300, session: 'slack-incidents-c7f3e1', createdBy: 'gateway' },
  { name: 'chat-explain-outbox-d4k8m', agent: 'coordinator', type: 'ai', phase: 'Succeeded', repo: 'checkout', createdAgo: 35, durationMin: 1, prompt: 'Explain how the outbox relay in checkout-service guarantees at-least-once delivery.', inputTokens: 21500, outputTokens: 1900, session: 'chat-maya-9b21f0', createdBy: 'chat' },
]

/** Sessions: chat transcripts and gateway conversation threads. */
export const SESSIONS = {
  slackIncidents: { name: 'slack-incidents-c7f3e1', agent: 'incident' as AgentKey, source: 'gateway', title: '#incidents: checkout latency spike' },
  chatMaya: { name: 'chat-maya-9b21f0', agent: 'coordinator' as AgentKey, source: 'chat', title: 'How does the outbox relay work?' },
  reviewThread: { name: 'github-pr-1479-a3e8d2', agent: 'reviewer' as AgentKey, source: 'gateway', title: 'checkout-service#1479 review thread' },
  releaseChat: { name: 'chat-release-v2-14-4d77aa', agent: 'coordinator' as AgentKey, source: 'chat', title: 'Release v2.14.0 planning' },
} as const

/** Find a task seed by name. */
export const taskSeed = (name: string) => TASKS.find((task) => task.name === name)
