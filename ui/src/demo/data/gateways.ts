import type { Gateway, GatewayBinding, GatewayDelivery, GatewayEvent } from '@/schemas/gateway'
import { ago, DEMO_NAMESPACE, NOW, digest, uid } from '../shared'
import { AGENTS, ORG, SESSIONS } from '../world'

// Northwind's external conversation plane: a Slack app in #incidents and
// #platform-help, GitHub webhooks for pull request reviews, and a Microsoft
// Teams support desk whose adapter is currently unreachable.

const SLACK_TEAM = 'T04NWLABS2'
const CHANNELS = { incidents: 'C05INCDNT1', platformHelp: 'C03PLATHLP' }
const SLACK_USERS = { maya: 'U02MCHEN01', dev: 'U03DPATEL1', sam: 'U04SOKAFR1', lena: 'U05LFSCHR1', jordan: 'U06JREYES1' }
const TEAMS_TENANT = '6f1c9a52-3d84-4b7e-9c21-0e5ad7b3f418'
const TEAMS_CHANNEL = '19:support-desk-4c1e@thread.tacv2'

/** Slack message timestamp for a moment `minutes` before page load. */
const slackTs = (minutes: number) => ((NOW - minutes * 60_000) / 1000).toFixed(6)

const conditions = (ready: boolean, at: string, reason = 'Ready', message?: string) =>
  ['Accepted', 'ResolvedRefs', 'Programmed', 'Ready'].map((type) => {
    const ok = type !== 'Ready' || ready
    return { type, status: ok ? 'True' : 'False', reason: ok ? type : reason, message: ok ? undefined : message, lastTransitionTime: at, observedGeneration: 1 }
  })

const SLACK_CAPABILITIES = { inboundText: true, outboundText: true, threads: true, senderIdentity: true, idempotentDelivery: true, interimDelivery: true }
const GITHUB_CAPABILITIES = { inboundText: true, outboundText: true, threads: true, senderIdentity: true, explicitSessions: true, idempotentDelivery: true }
const TEAMS_CAPABILITIES = { inboundText: true, outboundText: true, threads: true, senderIdentity: true }

function gateway(name: string, className: string, service: string, createdDaysAgo: number, status: Gateway['status'], generation = 3): Gateway {
  return {
    metadata: { name, namespace: DEMO_NAMESPACE, uid: uid(`gateway:${name}`), generation, creationTimestamp: ago(createdDaysAgo * 24 * 60) },
    spec: { gatewayClassName: className, adapter: { serviceRef: { name: service, port: 8090 } }, metadata: { team: 'platform' } },
    status: { observedGeneration: generation, resolvedEndpoint: `https://${service}.${DEMO_NAMESPACE}.svc:8090`, ...status },
  }
}

export const gateways: Gateway[] = [
  gateway('slack-northwind', 'slack', 'slack-adapter', 63, {
    accepted: true, resolvedRefs: true, connected: true, ready: true, lastSuccessfulProbe: ago(0.4),
    observedCapabilities: { contractVersion: 'orka.gateway.v1', adapterName: 'orka-slack-adapter', adapterVersion: 'v0.6.2', capabilities: SLACK_CAPABILITIES },
  }),
  gateway('github-northwind', 'github-webhooks', 'github-adapter', 48, {
    accepted: true, resolvedRefs: true, connected: true, ready: true, lastSuccessfulProbe: ago(0.7),
    observedCapabilities: { contractVersion: 'orka.gateway.v1', adapterName: 'orka-github-adapter', adapterVersion: 'v0.4.0', capabilities: GITHUB_CAPABILITIES },
  }, 5),
  gateway('teams-support', 'msteams', 'teams-adapter', 12, {
    accepted: true, resolvedRefs: true, connected: false, ready: false, lastSuccessfulProbe: ago(3 * 60 + 12),
    message: 'Adapter probe failed: dial tcp 10.0.14.22:8090: i/o timeout',
    observedCapabilities: { contractVersion: 'orka.gateway.v1', adapterName: 'orka-teams-adapter', adapterVersion: 'v0.2.1', capabilities: TEAMS_CAPABILITIES },
  }, 2),
]

interface BindingInput {
  name: string
  gateway: string
  agent: string
  match: GatewayBinding['spec']['match']
  senderPolicy?: GatewayBinding['spec']['senderPolicy']
  priority: number
  session: string
  taskDefaults?: GatewayBinding['spec']['taskDefaults']
  capabilities: typeof SLACK_CAPABILITIES | typeof GITHUB_CAPABILITIES | typeof TEAMS_CAPABILITIES
  createdDaysAgo: number
  lastInbound?: number
  lastOutbound?: number
  notReady?: string
}

function binding(input: BindingInput): GatewayBinding {
  const created = ago(input.createdDaysAgo * 24 * 60)
  return {
    metadata: { name: input.name, namespace: DEMO_NAMESPACE, uid: uid(`gatewaybinding:${input.name}`), generation: 1, creationTimestamp: created },
    spec: {
      gatewayRef: { name: input.gateway },
      agentRef: { name: input.agent },
      match: input.match,
      senderPolicy: input.senderPolicy,
      priority: input.priority,
      session: { mode: input.session },
      taskDefaults: input.taskDefaults,
      activeTurnBehavior: 'queue',
    },
    status: {
      accepted: true,
      resolvedRefs: true,
      programmed: true,
      ready: !input.notReady,
      observedGeneration: 1,
      resolvedCapabilities: input.capabilities,
      lastInboundActivity: input.lastInbound === undefined ? undefined : ago(input.lastInbound),
      lastOutboundActivity: input.lastOutbound === undefined ? undefined : ago(input.lastOutbound),
      message: input.notReady,
      conditions: conditions(!input.notReady, created, 'GatewayNotReady', input.notReady),
    },
  }
}

export const gatewayBindings: GatewayBinding[] = [
  binding({
    name: 'slack-incidents', gateway: 'slack-northwind', agent: AGENTS.incident.name,
    match: { accountId: SLACK_TEAM, contextId: CHANNELS.incidents }, senderPolicy: { mode: 'all' }, priority: 100, session: 'thread',
    taskDefaults: { priority: 10, timeout: '20m', retryPolicy: { maxRetries: 1, backoffMultiplier: 2, initialDelay: '15s' } },
    capabilities: SLACK_CAPABILITIES, createdDaysAgo: 63, lastInbound: 1.8, lastOutbound: 3.9,
  }),
  binding({
    name: 'slack-platform-help', gateway: 'slack-northwind', agent: AGENTS.coordinator.name,
    match: { accountId: SLACK_TEAM, contextId: CHANNELS.platformHelp },
    senderPolicy: { mode: 'allowlist', allowedSenderIds: Object.values(SLACK_USERS) }, priority: 50, session: 'thread-sender',
    taskDefaults: { timeout: '15m', retryPolicy: { maxRetries: 1 } },
    capabilities: SLACK_CAPABILITIES, createdDaysAgo: 40, lastInbound: 0.6, lastOutbound: 186,
  }),
  binding({
    name: 'github-checkout-reviews', gateway: 'github-northwind', agent: AGENTS.reviewer.name,
    match: { accountId: ORG, contextId: 'checkout-service' }, senderPolicy: { mode: 'all' }, priority: 100, session: 'thread',
    taskDefaults: { timeout: '30m', agentRuntimeMaxTurns: 40, retryPolicy: { maxRetries: 2, backoffMultiplier: 2, initialDelay: '30s' } },
    capabilities: GITHUB_CAPABILITIES, createdDaysAgo: 48, lastInbound: 1.2, lastOutbound: 46,
  }),
  binding({
    name: 'github-storefront-reviews', gateway: 'github-northwind', agent: AGENTS.reviewer.name,
    match: { accountId: ORG, contextId: 'web-storefront' }, senderPolicy: { mode: 'all' }, priority: 100, session: 'thread',
    taskDefaults: { timeout: '30m', agentRuntimeMaxTurns: 40 },
    capabilities: GITHUB_CAPABILITIES, createdDaysAgo: 31, lastInbound: 0.3, lastOutbound: 91,
  }),
  binding({
    name: 'teams-support-desk', gateway: 'teams-support', agent: AGENTS.triage.name,
    match: { accountId: TEAMS_TENANT, contextId: TEAMS_CHANNEL }, senderPolicy: { mode: 'all' }, priority: 10, session: 'context-sender',
    capabilities: TEAMS_CAPABILITIES, createdDaysAgo: 12, lastInbound: 3 * 60 + 5,
    notReady: 'Gateway teams-support is not ready: adapter probe failed',
  }),
]

const SESSION_1482 = 'github-pr-1482-5c9e1b'
const SESSION_873 = 'github-pr-873-0d4f7a'
const SESSION_879 = 'github-pr-879-2b7c41'

interface EventInput {
  key: string
  gateway: string
  binding?: string
  externalEventId: string
  state: GatewayEvent['state']
  stateMessage?: string
  accountId: string
  contextId: string
  threadId?: string
  senderId: string
  text: string
  replyTarget?: string
  sessionName?: string
  taskName?: string
  transcriptOrder?: number
  attemptCount?: number
  receivedAgo: number
  updatedAgo?: number
}

const gatewayUID = (name: string) => uid(`gateway:${name}`)

function event(input: EventInput): GatewayEvent {
  return {
    id: `gev-${digest(`${gatewayUID(input.gateway)}/${input.externalEventId}`, 40)}`,
    namespace: DEMO_NAMESPACE,
    gatewayName: input.gateway,
    bindingName: input.binding,
    externalEventId: input.externalEventId,
    state: input.state,
    stateMessage: input.stateMessage,
    accountId: input.accountId,
    contextId: input.contextId,
    threadId: input.threadId,
    senderId: input.senderId,
    text: input.text,
    replyTarget: input.replyTarget,
    sessionName: input.sessionName,
    taskName: input.taskName,
    transcriptOrder: input.transcriptOrder,
    attemptCount: input.attemptCount ?? 1,
    receivedAt: ago(input.receivedAgo),
    expiresAt: ago(input.receivedAgo - 7 * 24 * 60),
    createdAt: ago(input.receivedAgo - 0.02),
    updatedAt: ago(input.updatedAgo ?? input.receivedAgo - 0.05),
  }
}

const incidentThread = slackTs(4.4)
const inc512Thread = slackTs(1500.5)

const slackIncident = (input: Omit<EventInput, 'gateway' | 'binding' | 'accountId' | 'contextId'>): EventInput =>
  ({ ...input, gateway: 'slack-northwind', binding: 'slack-incidents', accountId: SLACK_TEAM, contextId: CHANNELS.incidents })
const githubPR = (repo: 'checkout-service' | 'web-storefront', pr: number, input: Omit<EventInput, 'gateway' | 'binding' | 'accountId' | 'contextId' | 'threadId' | 'replyTarget'>): EventInput =>
  ({
    ...input,
    gateway: 'github-northwind',
    binding: repo === 'checkout-service' ? 'github-checkout-reviews' : 'github-storefront-reviews',
    accountId: ORG,
    contextId: repo,
    threadId: `pull/${pr}`,
    replyTarget: `github:${ORG}/${repo}#${pr}`,
  })

const eventInputs: EventInput[] = [
  githubPR('web-storefront', 879, {
    key: 'pr-879', externalEventId: 'a0f3b7c2-6e1d-11f1-8d4a-3c2b9e7f1a05', state: 'Dispatching', sessionName: SESSION_879,
    senderId: 'github:lena-fischer', text: 'Pull request #879 opened: Move cart drawer to server components', transcriptOrder: 1, receivedAgo: 0.3, updatedAgo: 0.2,
  }),
  {
    key: 'platform-help-1', gateway: 'slack-northwind', binding: 'slack-platform-help', externalEventId: 'Ev07RB2X9QKM',
    state: 'Accepted', accountId: SLACK_TEAM, contextId: CHANNELS.platformHelp, threadId: slackTs(0.6), senderId: SLACK_USERS.dev,
    text: '@release-captain how do I get read access to the prod-replica Postgres for debugging?', replyTarget: `slack:${CHANNELS.platformHelp}/${slackTs(0.6)}`,
    receivedAgo: 0.6,
  },
  githubPR('checkout-service', 1482, {
    key: 'pr-1482', externalEventId: '5d2c81e0-6e1d-11f1-9b07-f1a2c3d4e5f6', state: 'TaskCreated', sessionName: SESSION_1482,
    taskName: 'review-pr-1482-q8m4d', senderId: 'github:jordan-reyes', text: 'Pull request #1482 opened: Add idempotency keys to payment capture',
    transcriptOrder: 1, receivedAgo: 1.2, updatedAgo: 1.0,
  }),
  slackIncident({
    key: 'incident-followup', externalEventId: 'Ev07RAZ4M1PD', state: 'Queued', stateMessage: 'Waiting for the active turn',
    threadId: incidentThread, senderId: SLACK_USERS.maya, sessionName: SESSIONS.slackIncidents.name, transcriptOrder: 8,
    text: 'Also check whether the 14:00 deploy (checkout v3.41.0) changed the payment client timeouts', replyTarget: `slack:${CHANNELS.incidents}/${incidentThread}`,
    receivedAgo: 1.8,
  }),
  slackIncident({
    key: 'incident-latency', externalEventId: 'Ev07RAWQ8C3N', state: 'TaskCreated', threadId: incidentThread, senderId: SLACK_USERS.sam,
    sessionName: SESSIONS.slackIncidents.name, taskName: 'checkout-latency-investigation-7k2qp', transcriptOrder: 7,
    text: '@incident-responder p99 on checkout-service jumped from 180ms to 1.4s at 14:02 UTC. Can you correlate with deploys and DB metrics?',
    replyTarget: `slack:${CHANNELS.incidents}/${incidentThread}`, receivedAgo: 4.4, updatedAgo: 4.1,
  }),
  githubPR('checkout-service', 1479, {
    key: 'pr-1479', externalEventId: '1b9e44a2-6dfb-11f1-8a3e-7d6c5b4a3921', state: 'Completed', sessionName: SESSIONS.reviewThread.name,
    taskName: 'review-pr-1479-b7n2k', senderId: 'github:dev-patel', text: 'Pull request #1479 opened: Migrate order events to the outbox pattern',
    transcriptOrder: 1, receivedAgo: 52.4, updatedAgo: 45.6,
  }),
  githubPR('web-storefront', 873, {
    key: 'pr-873', externalEventId: '0c7d12f8-6df5-11f1-87b1-2e4f6a8b0c1d', state: 'Completed', sessionName: SESSION_873,
    taskName: 'review-pr-873-f5j9w', senderId: 'github:maya-chen', text: 'Pull request #873 opened: Lazy-load product carousel images',
    transcriptOrder: 1, receivedAgo: 95.4, updatedAgo: 90.8,
  }),
  {
    key: 'platform-help-rejected', gateway: 'slack-northwind', binding: 'slack-platform-help', externalEventId: 'Ev07R7Q2LZXA',
    state: 'Rejected', stateMessage: 'Sender is not allowlisted',
    accountId: SLACK_TEAM, contextId: CHANNELS.platformHelp, threadId: slackTs(142), senderId: 'U09CNTRCT7',
    text: '@release-captain can you restart the staging cluster for me?', receivedAgo: 142,
  },
  {
    key: 'teams-vpn', gateway: 'teams-support', binding: 'teams-support-desk', externalEventId: '1760536188421',
    state: 'DeadLettered', stateMessage: 'Binding not ready after 5 attempts',
    accountId: TEAMS_TENANT, contextId: TEAMS_CHANNEL, senderId: '29:1aHq7Zx3-lena.fischer', attemptCount: 5,
    text: 'The VPN keeps dropping when I connect to the build farm from the Berlin office', replyTarget: `teams:${TEAMS_CHANNEL}`,
    receivedAgo: 3 * 60 + 5, updatedAgo: 2 * 60 + 40,
  },
  githubPR('web-storefront', 861, {
    key: 'pr-861-comment', externalEventId: 'e41f0a9c-6c63-11f1-9f2d-8b7a6c5d4e3f', state: 'Expired',
    stateMessage: 'Expired before dispatch', senderId: 'github:jordan-reyes',
    text: '@code-reviewer please take another look after the latest push', receivedAgo: 2 * 24 * 60 + 310, updatedAgo: 2 * 24 * 60 - 1130,
  }),
  slackIncident({
    key: 'inc-512', externalEventId: 'Ev07QK1F0T6W', state: 'Completed', threadId: inc512Thread, senderId: SLACK_USERS.maya,
    sessionName: SESSIONS.slackIncidents.name, taskName: 'incident-summary-inc-512-v7s4q', transcriptOrder: 5,
    text: '@incident-responder please write up the post-incident summary for INC-512 from this thread and the PagerDuty timeline',
    replyTarget: `slack:${CHANNELS.incidents}/${inc512Thread}`, receivedAgo: 1500.4, updatedAgo: 1496.8,
  }),
]

/** Newest first, as the ledger lists them. */
export const gatewayEvents: GatewayEvent[] = eventInputs.map(event)

const eventByKey = (key: string) => gatewayEvents[eventInputs.findIndex((input) => input.key === key)]

interface DeliveryInput {
  event: string
  kind: GatewayDelivery['kind']
  state: GatewayDelivery['state']
  text: string
  attemptCount: number
  maxAttempts?: number
  providerMessageId?: string
  lastError?: string
  createdAgo: number
  updatedAgo: number
  nextAttemptIn?: number
}

function delivery(input: DeliveryInput): GatewayDelivery {
  const source = eventByKey(input.event)
  const delivered = input.state === 'Delivered'
  return {
    id: `gdl-${digest(`${source.id}/${source.taskName ?? input.event}/${input.kind}`, 40)}`,
    namespace: DEMO_NAMESPACE,
    gatewayName: source.gatewayName,
    bindingName: source.bindingName,
    eventId: source.id,
    taskName: source.taskName,
    sessionName: source.sessionName,
    kind: input.kind,
    state: input.state,
    replyTarget: source.replyTarget ?? `${source.gatewayName}:${source.contextId}`,
    text: input.text,
    attemptCount: input.attemptCount,
    maxAttempts: input.maxAttempts ?? 5,
    manualRetryCount: 0,
    nextAttemptAt: input.nextAttemptIn === undefined ? ago(input.updatedAgo) : ago(-input.nextAttemptIn),
    expiresAt: ago(input.createdAgo - 3 * 24 * 60),
    providerMessageId: input.providerMessageId,
    lastError: input.lastError,
    createdAt: ago(input.createdAgo),
    updatedAt: ago(input.updatedAgo),
    deliveredAt: delivered ? ago(input.updatedAgo) : undefined,
  }
}

export const gatewayDeliveries: GatewayDelivery[] = [
  delivery({
    event: 'incident-latency', kind: 'message', state: 'Delivered', attemptCount: 1, createdAgo: 3.9, updatedAgo: 3.9,
    providerMessageId: `slack:${CHANNELS.incidents}/${slackTs(3.9)}`,
    text: 'On it. Pulling the checkout-service deploy history and Postgres metrics since 13:45 UTC.',
  }),
  delivery({
    event: 'pr-1479', kind: 'final', state: 'Delivered', attemptCount: 1, createdAgo: 45.7, updatedAgo: 45.6,
    providerMessageId: 'github:pullrequestreview/2384710932',
    text: 'Reviewed #1479. Two blocking issues: the outbox relay acks before the transaction commits, and the new migration locks order_events. Details inline.',
  }),
  delivery({
    event: 'pr-873', kind: 'final', state: 'DeadLettered', attemptCount: 5, createdAgo: 90.9, updatedAgo: 71.3,
    lastError: 'GitHub API 502 Bad Gateway',
    text: 'Reviewed #873. Looks good overall; consider adding width/height to carousel images to avoid layout shift.',
  }),
  delivery({
    event: 'pr-861-comment', kind: 'error', state: 'Failed', attemptCount: 3, maxAttempts: 3, createdAgo: 2 * 24 * 60 - 1130, updatedAgo: 2 * 24 * 60 - 1150,
    lastError: 'GitHub API 403 Forbidden',
    text: 'Sorry, this request expired before an agent could pick it up. Please mention @code-reviewer again.',
  }),
  delivery({
    event: 'teams-vpn', kind: 'error', state: 'RetryScheduled', attemptCount: 3, maxAttempts: 8, createdAgo: 2 * 60 + 40, updatedAgo: 6, nextAttemptIn: 2.5,
    lastError: 'adapter unavailable: i/o timeout',
    text: 'Sorry, the support desk agent is unavailable right now. Your request has been logged; please try again shortly.',
  }),
  delivery({
    event: 'inc-512', kind: 'final', state: 'Delivered', attemptCount: 1, createdAgo: 1496.9, updatedAgo: 1496.8,
    providerMessageId: `slack:${CHANNELS.incidents}/${slackTs(1496.8)}`,
    text: 'Post-incident summary for INC-512 (inventory sync backlog) is ready: root cause was a stuck Kafka consumer after the broker upgrade; 3 follow-up actions filed.',
  }),
]
