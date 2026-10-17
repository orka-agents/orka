import type { TranscriptMessage } from '@/schemas/session'
import { currentRuntimePool } from './runtimes'
import { ago, digest, uid } from '../shared'
import { AGENTS, SESSIONS, type AgentKey } from '../world'

// Session transcripts for world.ts SESSIONS. A transcript is JSONL of chat
// messages; session event streams are built from the session's tasks.

export interface SessionFixture {
  name: string
  agent: AgentKey
  /** minutes before page load the session started */
  createdAgo: number
  messages: TranscriptMessage[]
  /** ACP sessions carry an execution-control record. */
  acp?: { runtime: string; generation: number }
}

const model = (agent: AgentKey) => AGENTS[agent].model

const assistant = (agent: AgentKey, minutesAgo: number, content: string, tokens: [number, number], toolCalls?: TranscriptMessage['toolCalls']): TranscriptMessage => ({
  role: 'assistant',
  content,
  timestamp: ago(minutesAgo),
  model: model(agent),
  inputTokens: tokens[0],
  outputTokens: tokens[1],
  ...(toolCalls && { toolCalls }),
})

const user = (minutesAgo: number, content: string, name?: string): TranscriptMessage => ({
  role: 'user',
  content,
  timestamp: ago(minutesAgo),
  ...(name && { name }),
})

const toolResult = (minutesAgo: number, name: string, toolCallID: string, content: unknown): TranscriptMessage => ({
  role: 'tool',
  name,
  toolCallID,
  content: JSON.stringify(content),
  timestamp: ago(minutesAgo),
})

const call = (name: string, args: Record<string, unknown>) => ({ id: `call_${digest(`${name}:${JSON.stringify(args)}`, 12)}`, name, arguments: args })

const inc512Timeline = call('pagerduty_incident', { incident: 'INC-512', action: 'get_timeline' })
const inc512Thread = call('web_fetch', { url: 'https://northwind.slack.example/archives/C04INCIDENTS/p1760432110' })
const latencyTask = call('create_task', { agent: AGENTS.incident.name, prompt: 'p99 latency on checkout-service jumped from 180ms to 1.4s at 14:02 UTC. Correlate with recent deploys and database metrics, and propose a mitigation.' })
const outboxTask = call('create_task', { agent: AGENTS.coordinator.name, prompt: 'Explain how the outbox relay in checkout-service guarantees at-least-once delivery.' })
const releaseTask = call('create_task', { agent: AGENTS.coordinator.name, prompt: 'Prepare release v2.14.0 of web-storefront: collect merged PRs since v2.13.2, check for blocking issues, draft release notes, and update the docs.' })

export const SESSION_FIXTURES: SessionFixture[] = [
  {
    name: SESSIONS.slackIncidents.name,
    agent: 'incident',
    createdAgo: 1504,
    messages: [
      user(1503, '@orka write up the post-incident summary for INC-512 from this thread and the PagerDuty timeline', 'dev.patel'),
      assistant('incident', 1502, '', [9200, 410], [inc512Timeline, inc512Thread]),
      toolResult(1502, 'pagerduty_incident', inc512Timeline.id, { entries: 23 }),
      toolResult(1501, 'web_fetch', inc512Thread.id, { messages: 86 }),
      assistant('incident', 1499, 'Drafted the INC-512 summary and attached it to the PagerDuty incident.\n\n47 minutes, SEV-2: the stock-events consumer rejected every message after a schema change (registry compatibility was NONE). Three action items with owners are in the doc.', [31000, 3900]),
      user(1497, 'thanks! assigning the action items now', 'dev.patel'),
      user(4, '@orka checkout p99 just went from ~180ms to 1.4s at 14:02. can you dig in?', 'sam.okafor'),
      assistant('incident', 4, 'On it. Correlating the spike with today\'s deploys and database metrics.', [2100, 90], [latencyTask]),
      toolResult(4, 'create_task', latencyTask.id, { task: 'checkout-latency-investigation-7k2qp', phase: 'Pending' }),
      assistant('incident', 1, 'p99 spike lines up with #1477 (DB pool max 50 → 20): waiting Postgres connections went from 0 to 37 at 14:02. Requesting approval to roll back checkout-service to v3.18.2.', [16020, 301]),
    ],
  },
  {
    name: SESSIONS.chatMaya.name,
    agent: 'coordinator',
    createdAgo: 36,
    messages: [
      user(36, 'How does the outbox relay in checkout-service guarantee at-least-once delivery?'),
      assistant('coordinator', 35, '', [1900, 120], [outboxTask]),
      toolResult(34, 'create_task', outboxTask.id, { task: 'chat-explain-outbox-d4k8m', phase: 'Succeeded' }),
      assistant('coordinator', 34, 'Short version: the order and its event are written in one transaction, the relay only marks an event published after Kafka acknowledges it, and consumers dedupe on the event ID. A crash between publish and mark re-sends the event, which is the "at least once".', [16300, 1640]),
      user(31, 'Does that keep events for the same order in order?'),
      assistant('coordinator', 31, 'Yes, per order. The relay uses the order ID as the Kafka partition key and claims rows oldest-first, so one order\'s events land on one partition in commit order. Events for different orders can interleave.', [18400, 310]),
    ],
  },
  {
    name: SESSIONS.reviewThread.name,
    agent: 'reviewer',
    createdAgo: 55,
    acp: { runtime: 'claude', generation: 2 },
    messages: [
      user(55, 'Pull request #1479 opened by jordan.reyes: "Migrate order events to the outbox pattern" (+412 −96, 7 files)', 'github'),
      assistant('reviewer', 47, 'Approved #1479. Order events are written to the outbox in the order transaction and the relay uses FOR UPDATE SKIP LOCKED, so delivery is at-least-once and consumers already dedupe. Two non-blocking nits: log the relay batch at debug, and add a partial index on unpublished rows.', [91200, 6230]),
      user(22, 'good call on the partial index, added in 9c2e1f0. mind re-checking?', 'jordan.reyes'),
      assistant('reviewer', 20, 'Re-checked 9c2e1f0: `outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL` matches the relay\'s poll query. Still approved.', [24100, 280]),
    ],
  },
  {
    name: SESSIONS.releaseChat.name,
    agent: 'coordinator',
    createdAgo: 27,
    messages: [
      user(27, 'Let\'s get web-storefront v2.14.0 out today. Can you plan it, check blockers, and get the notes drafted?'),
      assistant('coordinator', 27, '', [2400, 160], [releaseTask]),
      toolResult(26, 'create_task', releaseTask.id, { task: 'release-v2-14-0-plan-r2w8z', phase: 'Pending' }),
      assistant('coordinator', 26, 'Started release-v2-14-0-plan-r2w8z. release-captain will collect the merged PRs, have issue-triager check blockers and docs-writer draft the notes, and report back here.', [2900, 110]),
    ],
  },
]

/** Execution-control record for an ACP-backed session. */
export function sessionExecutionControl(name: string, acp: NonNullable<SessionFixture['acp']>, createdAgo: number) {
  const sessionUID = uid(`session:${name}`)
  const pool = currentRuntimePool(acp.runtime)
  return {
    resourceVersion: String(48_211 + acp.generation * 7),
    sessionUID,
    runtimePoolRef: pool.metadata.name,
    runtimeProfileDigest: pool.spec.runtime.profile.digest,
    generation: acp.generation,
    lifecycle: 'Idle',
    availability: 'Available' as const,
    mutationLeaseGeneration: acp.generation,
    lineage: {
      namespaceUID: uid('namespace:default'),
      sessionUID,
      contractVersion: 'orka.harness.v2' as const,
      generation: acp.generation,
      runtimeIdentity: `${acp.runtime}@acp-${acp.runtime}`,
      configDigest: `sha256:${digest(`config:${name}`, 64)}`,
      establishedAt: ago(createdAgo - 1),
    },
  }
}
