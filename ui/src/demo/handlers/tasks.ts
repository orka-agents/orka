import { http, HttpResponse, type RequestHandler } from 'msw'
import type { Task, TaskPhase } from '@/schemas/task'
import type { Approval, ExecutionEvent, ForkTaskRequest, TaskTrace } from '@/schemas/execution-event'
import type { ArtifactMetadata } from '@/schemas/artifact'
import type { TranscriptMessage } from '@/schemas/session'
import {
  AMBIENT,
  CONTAINERS,
  EXTRA_SESSION_LINKS,
  STORIES,
  liveStory,
  type Beat,
  type EventSpec,
  type Outcome,
  type Story,
} from '../data/tasks'
import { currentRuntimePool } from '../data/runtimes'
import { SESSION_FIXTURES, sessionExecutionControl } from '../data/sessions'
import { api, apiError, DEMO_NAMESPACE, digest, inDemoNamespace, list, noContent, notFound, NOW, query, sseFrame, taskUID, uid, isDNSLabel } from '../shared'
import { AGENTS, REPOS, TASKS, type AgentKey, type DemoTaskSeed, type RepoKey } from '../world'

// The demo's task engine. Every task is a record that plays its story (see
// data/tasks.ts) against the wall clock: beats whose time has come are applied
// lazily whenever the API is read, so there are no background timers. A beat
// that opens an approval blocks the story until the visitor decides it.

const TERMINAL = new Set<TaskPhase>(['Succeeded', 'Failed', 'Cancelled'])
const NAMESPACE_UID = uid('namespace:default')

interface Scheduled {
  at: number
  beat: Beat
}

interface TaskRecord {
  name: string
  uid: string
  type: 'ai' | 'agent' | 'container'
  agent?: AgentKey
  /** Agent name when the task names an agent outside world.ts. */
  agentName?: string
  repo?: RepoKey
  prompt: string
  session?: string
  parent?: string
  createdBy: string
  created: number
  start: number
  /** A cron parent: never runs itself. */
  schedule?: { cron: string; timeZone: string }
  timeline: Scheduled[]
  cursor: number
  started: boolean
  finished: boolean
  /** When the outcome applies; moves when an approval gate opens and closes. */
  end: number
  outcome: Outcome
  declined?: Story['declined']
  gate?: { approvalId: string; at: number }
  tokenFactor: { input: number; output: number }
  events: ExecutionEvent[]
  logs: string[]
  approvals: Approval[]
  artifacts: Array<ArtifactMetadata & { body: string }>
  labels: Record<string, string>
  annotations: Record<string, string>
  ownerUID?: string
  /** Fields from a POST /tasks body echoed back on the spec. */
  specExtras?: Partial<Task['spec']>
}

interface SessionRecord {
  name: string
  agent?: AgentKey
  created: number
  messages: TranscriptMessage[]
  acp?: { runtime: string; generation: number }
}

const tasks = new Map<string, TaskRecord>()
const sessions = new Map<string, SessionRecord>()

const iso = (ms: number) => new Date(ms).toISOString()
const agentNameOf = (rec: TaskRecord) => rec.agentName ?? (rec.agent ? AGENTS[rec.agent].name : undefined)
const runtimeOf = (rec: TaskRecord) => {
  const agent = rec.agent ? AGENTS[rec.agent] : undefined
  return agent && 'runtime' in agent ? agent.runtime : 'claude'
}
const writeIntent = (rec: TaskRecord) => rec.type === 'agent' && rec.agent !== 'reviewer'
const randomSuffix = () => Math.random().toString(36).slice(2, 7).padEnd(5, 'x')

// ---- scheduling --------------------------------------------------------------

/** Spreads beats over [from, to] in proportion to their gaps; the last lands on `to`. */
function spread(beats: Beat[], from: number, to: number): Scheduled[] {
  const total = beats.reduce((sum, beat) => sum + Math.max(beat.gap, 0.1), 0)
  let acc = 0
  return beats.map((beat) => {
    acc += Math.max(beat.gap, 0.1)
    return { at: Math.round(from + (acc / total) * (to - from)), beat }
  })
}

/** Places beats at their own gaps (seconds) after `from`. */
function sequence(beats: Beat[], from: number): Scheduled[] {
  let at = from
  return beats.map((beat) => {
    at += Math.max(beat.gap, 0.2) * 1000
    return { at, beat }
  })
}

function tokenTotals(beats: Beat[]) {
  let input = 0
  let output = 0
  for (const beat of beats) {
    for (const event of beat.events ?? []) {
      if (event.type !== 'ModelRequestCompleted') continue
      input += event.inputTokens ?? 0
      output += event.outputTokens ?? 0
    }
  }
  return { input, output }
}

function baseRecord(fields: Omit<TaskRecord, 'uid' | 'cursor' | 'started' | 'finished' | 'events' | 'logs' | 'approvals' | 'artifacts' | 'tokenFactor' | 'labels' | 'annotations'> & Partial<Pick<TaskRecord, 'tokenFactor' | 'labels' | 'annotations'>>): TaskRecord {
  const agentName = fields.agentName ?? (fields.agent ? AGENTS[fields.agent].name : undefined)
  return {
    uid: taskUID(fields.name, fields.created),
    cursor: 0,
    started: false,
    finished: false,
    events: [],
    logs: [],
    approvals: [],
    artifacts: [],
    tokenFactor: { input: 1, output: 1 },
    ...fields,
    labels: {
      'orka.ai/task-type': fields.type,
      ...(agentName && { 'orka.ai/agent': agentName }),
      ...fields.labels,
    },
    annotations: {
      'orka.ai/created-by': fields.createdBy,
      ...fields.annotations,
    },
  }
}

function seedRecord(seed: DemoTaskSeed): TaskRecord {
  const created = NOW - seed.createdAgo * 60_000
  const session = seed.session ?? EXTRA_SESSION_LINKS[seed.name]
  const common = {
    name: seed.name,
    type: seed.type,
    agent: seed.agent,
    repo: seed.repo,
    prompt: seed.prompt,
    session,
    parent: seed.parent,
    createdBy: seed.createdBy ?? 'maya.chen',
    created,
  }

  if (seed.phase === 'Scheduled') {
    return baseRecord({
      ...common,
      start: Number.POSITIVE_INFINITY,
      end: Number.POSITIVE_INFINITY,
      schedule: { cron: '0 9 * * 4', timeZone: 'Europe/Berlin' },
      timeline: [],
      outcome: { phase: 'Succeeded' },
    })
  }

  const story = STORIES[seed.name]
  if (!story) throw new Error(`demo: no story for task ${seed.name}`)
  const queueMs = seed.type === 'container' ? 4_000 : 8_000
  let start = created + queueMs
  let timeline: Scheduled[]
  let end: number

  const timing = story.timing
  if (timing?.kind === 'pending') {
    start = NOW + timing.startInSec * 1000
    timeline = spread(story.beats, start, start + timing.runSec * 1000)
    end = timeline[timeline.length - 1].at + 1500
  } else if (timing?.kind === 'running') {
    const history = spread(story.beats.slice(0, timing.played), start, NOW - 3_000)
    const live = spread(story.beats.slice(timing.played), NOW, NOW + timing.remainingSec * 1000)
    timeline = [...history, ...live]
    end = timeline[timeline.length - 1].at + 1500
  } else {
    end = created + (seed.durationMin ?? 2) * 60_000
    timeline = spread(story.beats, start, end - 1500)
  }

  const totals = tokenTotals(story.beats)
  const record = baseRecord({
    ...common,
    start,
    end,
    timeline,
    outcome: story.outcome,
    declined: story.declined,
    tokenFactor: {
      input: seed.inputTokens && totals.input ? seed.inputTokens / totals.input : 1,
      output: seed.outputTokens && totals.output ? seed.outputTokens / totals.output : 1,
    },
  })
  if (seed.parent) linkParent(record, seed.parent)
  return record
}

function linkParent(record: TaskRecord, parentName: string) {
  const parent = tasks.get(parentName) ?? seedParent(parentName)
  record.labels['orka.ai/parent-task'] = parentName
  record.annotations['orka.ai/parent-task-name'] = parentName
  if (parent?.schedule) record.labels['orka.ai/scheduled-run'] = 'true'
  record.ownerUID = parent?.uid
}

/** Parents are created before their children; this covers catalog order. */
function seedParent(name: string) {
  const seed = TASKS.find((task) => task.name === name)
  if (!seed) return undefined
  const parent = seedRecord(seed)
  tasks.set(parent.name, parent)
  return parent
}

// ---- playing stories -----------------------------------------------------------

function emit(rec: TaskRecord, spec: EventSpec, at: number) {
  const seq = rec.events.length + 1
  const event: ExecutionEvent = {
    id: `evt-${digest(`${rec.uid}:${seq}`, 20)}`,
    namespace: DEMO_NAMESPACE,
    streamType: 'task',
    streamID: rec.name,
    seq,
    type: spec.type,
    severity: spec.severity ?? 'info',
    taskName: rec.name,
    agentName: agentNameOf(rec),
    sessionName: rec.session,
    toolName: spec.toolName,
    toolCallID: spec.toolCallID,
    provider: spec.provider,
    model: spec.model,
    stopReason: spec.stopReason,
    summary: spec.summary,
    content: spec.content,
    contentText: spec.contentText,
    createdAt: iso(at),
  }
  if (spec.type === 'ModelRequestCompleted') {
    const input = Math.round((spec.inputTokens ?? 0) * rec.tokenFactor.input)
    const output = Math.round((spec.outputTokens ?? 0) * rec.tokenFactor.output)
    const window = spec.contextWindowSize ?? 200_000
    Object.assign(event, {
      inputTokens: input,
      outputTokens: output,
      cachedInputTokens: Math.round((spec.cachedInputTokens ?? 0) * rec.tokenFactor.input),
      contextWindowUsed: Math.min(input + output, Math.round(window * 0.97)),
      contextWindowSize: window,
      summary: `${spec.model} · ${input.toLocaleString('en-US')} in / ${output.toLocaleString('en-US')} out · ${spec.stopReason}`,
    })
  }
  rec.events.push(event)
  if (rec.type === 'ai') rec.logs.push(workerLogLine(event))
}

/** The AI worker's structured log line for an event. */
function workerLogLine(event: ExecutionEvent): string {
  const level = event.severity === 'error' ? 'ERROR' : event.severity === 'warning' ? 'WARN' : 'INFO'
  const fields: Record<string, unknown> = { task: event.taskName }
  let message = event.summary ?? event.type
  switch (event.type) {
    case 'ModelRequestStarted':
      message = 'model request'
      Object.assign(fields, { provider: event.provider, model: event.model })
      break
    case 'ModelRequestCompleted':
      message = 'model response'
      Object.assign(fields, { stopReason: event.stopReason, inputTokens: event.inputTokens, outputTokens: event.outputTokens })
      break
    case 'ToolCallStarted':
      message = 'tool call'
      Object.assign(fields, { tool: event.toolName, id: event.toolCallID })
      break
    case 'ToolCallCompleted':
      message = 'tool result'
      Object.assign(fields, { tool: event.toolName, id: event.toolCallID, summary: event.summary })
      break
    case 'ToolCallFailed':
      message = 'tool failed'
      Object.assign(fields, { tool: event.toolName, id: event.toolCallID, error: event.summary })
      break
    case 'ModelMessage':
      message = 'assistant message'
      fields.chars = event.contentText?.length ?? 0
      break
  }
  return `${event.createdAt}\t${level}\tworker\t${message}\t${JSON.stringify(fields)}`
}

function play(rec: TaskRecord, { at, beat }: Scheduled) {
  beat.events?.forEach((spec, index) => emit(rec, spec, at + index * 150))
  if (beat.logs) {
    const prefix = rec.type === 'ai' ? `${iso(at)}\tINFO\tworker\t` : ''
    rec.logs.push(...beat.logs.map((line) => (prefix && line ? prefix + line : line)))
  }
  if (beat.artifact) {
    rec.artifacts.push({
      filename: beat.artifact.filename,
      contentType: beat.artifact.contentType,
      size: new TextEncoder().encode(beat.artifact.body).length,
      createdAt: iso(at),
      body: beat.artifact.body,
    })
  }
  if (beat.approval) {
    const spec = beat.approval
    const approval: Approval = {
      id: spec.id,
      action: spec.action,
      riskSummary: spec.riskSummary,
      targetTool: spec.targetTool,
      targetArgsPreview: spec.targetArgsPreview,
      targetArgsDigest: `sha256:${digest(`${spec.id}:args`, 64)}`,
      toolCallID: spec.toolCallID,
      status: 'pending',
      createdAt: iso(at),
      timeout: spec.timeout ?? '1h',
      expiresAt: iso(at + durationMs(spec.timeout ?? '1h')),
    }
    if (spec.decision) {
      Object.assign(approval, {
        status: spec.decision.status,
        decisionTime: iso(at + spec.decision.afterSec * 1000),
        decisionActor: spec.decision.actor,
        decisionReason: spec.decision.reason,
        decisionSeq: rec.events.length + 1,
      })
    } else {
      rec.gate = { approvalId: spec.id, at }
    }
    rec.approvals.push(approval)
  }
}

/** Milliseconds in a simple Go-style duration such as "30m" or "1h". */
function durationMs(duration: string) {
  const match = duration.match(/^(\d+)([smh])$/)
  if (!match) return 60 * 60_000
  return Number(match[1]) * { s: 1000, m: 60_000, h: 3_600_000 }[match[2] as 's' | 'm' | 'h']
}

function startTask(rec: TaskRecord) {
  rec.started = true
  if (rec.type === 'agent') {
    emit(rec, { type: 'TaskStarted', summary: `Dispatched to RuntimePool ${currentRuntimePool(runtimeOf(rec)).metadata.name}` }, rec.start)
    return
  }
  emit(rec, { type: 'TaskJobCreated', summary: `Created Job ${jobName(rec)}` }, rec.start - 400)
  emit(rec, { type: 'TaskStarted', summary: 'Pod started (attempt 1)' }, rec.start)
  if (rec.type === 'container') {
    const spec = containerSpec(rec)
    rec.logs.push(`+ ${[...spec.command, ...(spec.args ?? [])].join(' ')}`)
  }
}

function finishTask(rec: TaskRecord) {
  rec.finished = true
  const { phase, message } = rec.outcome
  const seconds = Math.max(1, Math.round((rec.end - rec.start) / 1000))
  const took = seconds >= 60 ? `${Math.floor(seconds / 60)}m${String(seconds % 60).padStart(2, '0')}s` : `${seconds}s`
  if (phase === 'Succeeded') emit(rec, { type: 'TaskSucceeded', summary: `Task succeeded in ${took}` }, rec.end)
  else if (phase === 'Failed') emit(rec, { type: 'TaskFailed', severity: 'error', summary: message ?? 'Task failed' }, rec.end)
  else emit(rec, { type: 'TaskCancelled', severity: 'warning', summary: message ?? 'Task cancelled' }, rec.end)
  if (rec.outcome.pr) {
    const pr = rec.outcome.pr
    Object.assign(rec.annotations, {
      'orka.ai/pr-url': `${REPOS[rec.repo ?? 'checkout'].url}/pull/${pr.number}`,
      'orka.ai/pr-number': String(pr.number),
      'orka.ai/pr-status': pr.status,
    })
  }
}

function advance(rec: TaskRecord, now: number) {
  if (rec.finished) return
  if (!rec.events.length) {
    const by = rec.createdBy
    const source = by === 'gateway' || by === 'monitor' || by === 'schedule' || by === 'chat' ? `the ${by}` : by
    emit(rec, { type: 'TaskCreated', summary: `Task created by ${source}` }, rec.created)
  }
  if (rec.schedule || now < rec.start) return
  if (!rec.started) startTask(rec)
  while (!rec.gate && rec.cursor < rec.timeline.length && rec.timeline[rec.cursor].at <= now) {
    play(rec, rec.timeline[rec.cursor])
    rec.cursor++
  }
  if (!rec.gate && rec.cursor >= rec.timeline.length && now >= rec.end) finishTask(rec)
}

// Background work keeps arriving while the demo is open.
let nextAmbientAt = NOW + 150_000
let ambientCount = 0
const AMBIENT_PERIOD_MS = 210_000

function advanceAll(now = Date.now()) {
  while (now >= nextAmbientAt) {
    // After a long idle stretch, skip the backlog instead of starting it all at once.
    if (now - nextAmbientAt < 10 * 60_000) spawnAmbient(nextAmbientAt)
    nextAmbientAt += AMBIENT_PERIOD_MS
  }
  for (const rec of tasks.values()) advance(rec, now)
}

function spawnAmbient(at: number) {
  const template = AMBIENT[ambientCount % AMBIENT.length]
  const n = ambientCount++
  const story = template.story(n)
  const issue = template.prompt(n).match(/#(\d+)/)?.[1]
  addLiveTask({
    name: `${template.prefix}${issue ? `-${issue}` : ''}-${randomSuffix()}`,
    type: template.type,
    agent: template.agent,
    repo: template.repo,
    prompt: template.prompt(n),
    createdBy: template.createdBy,
    story,
    created: at,
  })
}

function phaseOf(rec: TaskRecord, now = Date.now()): TaskPhase {
  if (rec.schedule) return 'Scheduled'
  if (rec.finished) return rec.outcome.phase
  return now < rec.start ? 'Pending' : 'Running'
}

// ---- approvals -----------------------------------------------------------------

function decide(rec: TaskRecord, approval: Approval, decision: 'approve' | 'decline', reason: string | undefined, now: number) {
  advance(rec, now)
  const approved = decision === 'approve'
  Object.assign(approval, {
    status: approved ? 'approved' : 'declined',
    decisionTime: iso(now),
    decisionActor: 'demo-user',
    decisionReason: reason || undefined,
    decisionSeq: rec.events.length + 1,
  })
  emit(rec, { type: approved ? 'ApprovalApproved' : 'ApprovalDeclined', summary: `${approved ? 'Approved' : 'Declined'} by demo-user${reason ? `: ${reason}` : ''}`, content: { approvalID: approval.id } }, now)
  if (rec.gate?.approvalId !== approval.id) return
  const gateAt = rec.gate.at
  rec.gate = undefined
  if (!approved && rec.declined) {
    rec.timeline = [...rec.timeline.slice(0, rec.cursor), ...sequence(rec.declined.beats, now)]
    rec.outcome = rec.declined.outcome
  } else {
    // Keep the rest of the story's pacing, starting from the decision.
    rec.timeline = rec.timeline.map((item, index) => (index < rec.cursor ? item : { ...item, at: now + Math.max(item.at - gateAt, 1500) }))
  }
  rec.end = (rec.timeline[rec.timeline.length - 1]?.at ?? now) + 1500
}

/** The event that recorded the task's outcome; later events (forks) do not replace it. */
function terminalEvent(rec: TaskRecord) {
  return [...rec.events].reverse().find((e) => e.type === 'TaskSucceeded' || e.type === 'TaskFailed' || e.type === 'TaskCancelled')
}

function isWaiting(rec: TaskRecord) {
  return Boolean(rec.gate) && !rec.finished
}

// ---- tasks created during the demo --------------------------------------------------

interface LiveTaskInput {
  name: string
  type: 'ai' | 'agent' | 'container'
  agent?: AgentKey
  agentName?: string
  repo?: RepoKey
  prompt: string
  session?: string
  parent?: string
  createdBy: string
  story?: Story
  created?: number
  labels?: Record<string, string>
  annotations?: Record<string, string>
  specExtras?: Partial<Task['spec']>
}

function addLiveTask(input: LiveTaskInput): TaskRecord {
  const created = input.created ?? Date.now()
  const start = created + 4_000
  const story = input.story ?? liveStory(input.type, {
    agent: input.agent,
    prompt: input.prompt,
    repo: input.repo,
    image: typeof input.specExtras?.image === 'string' ? input.specExtras.image : undefined,
  })
  const timeline = sequence(story.beats, start)
  const record = baseRecord({
    name: input.name,
    type: input.type,
    agent: input.agent,
    agentName: input.agentName,
    repo: input.repo,
    prompt: input.prompt,
    session: input.session,
    parent: input.parent,
    createdBy: input.createdBy,
    created,
    start,
    end: (timeline[timeline.length - 1]?.at ?? start) + 1500,
    timeline,
    outcome: story.outcome,
    declined: story.declined,
    labels: input.labels,
    annotations: input.annotations,
    specExtras: input.specExtras,
  })
  if (input.parent) linkParent(record, input.parent)
  if (input.session) ensureSession(input.session, input.agent, created)
  tasks.set(record.name, record)
  return record
}

export interface DemoTaskInput {
  agent: AgentKey
  prompt: string
  repo?: RepoKey
  /** session the task belongs to (e.g. the chat session that created it) */
  session?: string
  createdBy?: 'chat' | 'gateway' | 'monitor'
}

/**
 * Adds a new task to the in-memory store, starts its simulated lifecycle
 * (Pending -> Running -> Succeeded with events, logs, and a result), and
 * returns its name. Used by other demo domains, e.g. chat's create_task tool.
 */
export function createDemoTask(input: DemoTaskInput): string {
  const slug = input.prompt.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').split('-').slice(0, 4).join('-') || 'task'
  let name = `${slug}-${randomSuffix()}`
  while (tasks.has(name)) name = `${slug}-${randomSuffix()}`
  addLiveTask({
    name,
    type: AGENTS[input.agent].kind,
    agent: input.agent,
    repo: input.repo,
    prompt: input.prompt,
    session: input.session,
    createdBy: input.createdBy ?? 'chat',
  })
  return name
}

/**
 * Cancels a pending or running task now, the way a controller-side
 * cancellation (such as a repository monitor Stop command) does. Finished and
 * scheduled tasks are left alone.
 */
export function cancelDemoTask(name: string, reason: string) {
  const rec = tasks.get(name)
  const now = Date.now()
  if (!rec) return
  advance(rec, now)
  if (rec.finished || rec.schedule) return
  for (const approval of rec.approvals) {
    if (approval.status !== 'pending') continue
    Object.assign(approval, { status: 'cancelled', decisionTime: iso(now), decisionReason: reason })
    emit(rec, { type: 'ApprovalCancelled', severity: 'warning', summary: `Approval cancelled: ${reason}`, content: { approvalID: approval.id } }, now)
  }
  rec.gate = undefined
  rec.timeline = rec.timeline.slice(0, rec.cursor)
  if (!rec.started) rec.start = now
  rec.end = now
  rec.outcome = { phase: 'Cancelled', message: reason }
  finishTask(rec)
}

// ---- views ------------------------------------------------------------------------

function jobName(rec: TaskRecord) {
  return `${rec.name.slice(0, 52)}-${digest(rec.uid, 5)}`
}

function containerSpec(rec: TaskRecord) {
  const extras = rec.specExtras
  if (extras?.image) return { image: extras.image, command: extras.command ?? [], args: extras.args }
  return CONTAINERS[rec.name] ?? { image: 'hashicorp/terraform:1.9.8', command: ['terraform', 'plan'], args: ['-detailed-exitcode', '-input=false', '-lock=false'] }
}

function prNumberFromName(name: string) {
  return name.match(/^review-pr-(\d+)/)?.[1]
}

function taskSpec(rec: TaskRecord): Task['spec'] {
  const agentName = agentNameOf(rec)
  const sessionRef = rec.session ? { name: rec.session, append: true } : undefined
  const extras = rec.specExtras ?? {}
  if (rec.type === 'container') {
    const spec = containerSpec(rec)
    return { type: 'container', image: spec.image, command: spec.command, ...(spec.args && { args: spec.args }), timeout: '10m', ...pick(extras, ['priority', 'timeout']) }
  }
  if (rec.type === 'ai') {
    return {
      type: 'ai',
      ...(agentName && { agentRef: { name: agentName } }),
      ai: extras.ai ?? { prompt: rec.prompt },
      ...(sessionRef && { sessionRef }),
      timeout: '15m',
      ...pick(extras, ['priority', 'timeout']),
      ...(rec.schedule && { schedule: rec.schedule.cron, timeZone: rec.schedule.timeZone, concurrencyPolicy: 'Forbid' }),
    }
  }
  const repo = rec.repo ? REPOS[rec.repo] : undefined
  const pr = prNumberFromName(rec.name)
  const workspace: Task['spec']['workspace'] = extras.workspace ?? (repo
    ? writeIntent(rec)
      ? {
          intent: 'write',
          gitRepo: `${repo.url}.git`,
          branch: 'main',
          readCredentialRef: { name: 'github-read-token' },
          publicationCredentialRef: { name: 'github-app-checkout' },
          forgeCredentialRef: { name: 'github-app-checkout' },
          prBaseBranch: 'main',
          createPR: true,
        }
      : { intent: 'read', gitRepo: `${repo.url}.git`, ...(pr ? { ref: `refs/pull/${pr}/head` } : { branch: 'main' }), readCredentialRef: { name: 'github-read-token' } }
    : undefined)
  return {
    type: 'agent',
    ...(agentName && { agentRef: { name: agentName } }),
    prompt: rec.prompt,
    ...(workspace && { workspace }),
    ...(sessionRef && { sessionRef }),
    timeout: '15m',
    ...pick(extras, ['priority', 'timeout', 'agentRuntime']),
    ...(rec.schedule && { schedule: rec.schedule.cron, timeZone: rec.schedule.timeZone, concurrencyPolicy: 'Forbid' }),
  }
}

function pick<T extends object, K extends keyof T>(source: T, keys: K[]): Partial<Pick<T, K>> {
  const out: Partial<Pick<T, K>> = {}
  for (const key of keys) if (source[key] !== undefined) out[key] = source[key]
  return out
}

function latestPlan(rec: TaskRecord) {
  const event = [...rec.events].reverse().find((e) => e.type === 'PlanUpdated')
  if (!event) return undefined
  const content = (event.content ?? {}) as { iteration?: number; progressPct?: number; goalComplete?: boolean }
  return { summary: event.summary, progressPct: content.progressPct, goalComplete: content.goalComplete, planDocument: event.contentText, iteration: content.iteration }
}

function children(name: string) {
  return [...tasks.values()].filter((rec) => rec.parent === name)
}

function shortResult(rec: TaskRecord): string | undefined {
  if (!rec.finished) return undefined
  if (rec.outcome.phase !== 'Succeeded') return rec.outcome.message
  const raw = rec.outcome.result ?? ''
  let text = raw
  try {
    const parsed = JSON.parse(raw) as { summary?: string }
    if (parsed && typeof parsed.summary === 'string') text = parsed.summary
  } catch {
    // Plain text result.
  }
  const line = text.split('\n').map((l) => l.replace(/^[#>*\-\s]+/, '').replace(/\*\*/g, '').trim()).find(Boolean) ?? ''
  return line.length > 120 ? `${line.slice(0, 117)}…` : line
}

function toTask(rec: TaskRecord, now = Date.now()): Task {
  const phase = phaseOf(rec, now)
  const terminal = TERMINAL.has(phase)
  const plan = latestPlan(rec)
  const childRecords = children(rec.name)
  const waiting = isWaiting(rec)
  const pendingApproval = waiting ? rec.approvals.find((a) => a.id === rec.gate?.approvalId) : undefined
  const lastTransition = iso(rec.finished ? rec.end : rec.started ? rec.start : rec.created)

  const status: NonNullable<Task['status']> = { phase }
  if (rec.schedule) {
    const runs = childRecords.sort((a, b) => b.created - a.created)
    if (runs[0]) {
      status.lastScheduleTime = iso(runs[0].created)
      status.nextScheduleTime = iso(runs[0].created + 7 * 24 * 60 * 60_000)
    }
    status.conditions = [{ type: 'Scheduled', status: 'True', reason: 'CronActive', message: `Next run ${status.nextScheduleTime ?? 'pending'}`, lastTransitionTime: iso(rec.created) }]
  } else {
    if (rec.started) {
      status.startTime = iso(rec.start)
      status.attempts = 1
      if (rec.type !== 'agent') status.jobName = jobName(rec)
    }
    if (rec.finished) {
      status.completionTime = iso(rec.end)
      status.resultRef = { available: Boolean(rec.outcome.result) }
      if (rec.outcome.message) status.message = rec.outcome.message
    }
    if (pendingApproval) status.message = `waiting for approval ${pendingApproval.id}: ${pendingApproval.action}`
    status.conditions = [
      { type: 'Admitted', status: 'True', reason: 'PolicyAllowed', lastTransitionTime: iso(rec.created + 500) },
      ...(waiting ? [{ type: 'WaitingForApproval', status: 'True', reason: 'ApprovalPending', message: pendingApproval?.action, lastTransitionTime: pendingApproval?.createdAt }] : []),
      ...(terminal ? [{ type: 'Complete', status: 'True', reason: phase, message: rec.outcome.message, lastTransitionTime: lastTransition }] : []),
    ]
  }
  if (plan?.iteration) status.iteration = plan.iteration
  if (childRecords.length && !rec.schedule) {
    status.childTasks = childRecords.map((child) => ({
      name: child.name,
      agent: agentNameOf(child) ?? 'unassigned',
      phase: phaseOf(child, now),
      ...(shortResult(child) && { result: shortResult(child) }),
    }))
  }

  if (rec.type === 'agent' && !rec.schedule) {
    const runtime = runtimeOf(rec)
    const pool = currentRuntimePool(runtime)
    const state = phase === 'Pending' ? 'Queued' : phase === 'Running' ? 'Running' : phase === 'Cancelled' ? 'Cancelled' : phase === 'Failed' ? 'Failed' : 'Succeeded'
    status.execution = {
      state,
      ...(terminal && { outcome: state as 'Succeeded' | 'Failed' | 'Cancelled' }),
      attempt: 1,
      ...(rec.started && {
        promptID: `prompt-${digest(rec.uid, 10)}`,
        runtimePoolName: pool.metadata.name,
        runtimePoolUID: pool.metadata.uid,
        runtimeInstanceID: pool.status?.activeInstance?.runtimeInstanceID ?? `rti-${digest(`instance:${runtime}`, 24)}`,
        runtimeSessionUID: uid(`runtime-session:${rec.name}`),
        runtimeSessionGeneration: 1,
        requestDigest: `sha256:${digest(`request:${rec.name}`, 64)}`,
        controllerEpoch: 14,
      }),
      ...(phase === 'Failed' && { reason: 'PromptDeadlineExceeded', message: rec.outcome.message }),
      lastTransitionTime: lastTransition,
    }
    status.delivery = deliveryStatus(rec, phase)
    if (rec.started) {
      const agentName = agentNameOf(rec) ?? 'unknown'
      status.agentExecutionBinding = {
        schemaVersion: 1,
        contractVersion: 'orka.harness.v2',
        backend: 'runtime-pool',
        bindingDigest: `sha256:${digest(`binding:${rec.name}`, 64)}`,
        task: { namespaceUID: NAMESPACE_UID, uid: rec.uid, boundSpecGeneration: 1 },
        agent: { namespace: DEMO_NAMESPACE, name: agentName, uid: uid(`agent:${agentName}`), generation: 3 },
        snapshot: { id: `snap-${digest(`snapshot:${rec.name}`, 12)}`, digest: `sha256:${digest(`snapshot:${rec.name}`, 64)}`, schemaVersion: 2 },
        runtimeType: runtime,
        runtimeProfileDigest: `sha256:${digest(`profile:${runtime}`, 64)}`,
        runtimeProfileDigestSchemaVersion: 1,
        boundAt: iso(rec.start - 1000),
      }
    }
  }

  const ownerReferences = rec.ownerUID && rec.parent
    ? [{ apiVersion: 'core.orka.ai/v1alpha1', kind: 'Task', name: rec.parent, uid: rec.ownerUID, controller: true }]
    : undefined

  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'Task',
    metadata: {
      name: rec.name,
      namespace: DEMO_NAMESPACE,
      uid: rec.uid,
      creationTimestamp: iso(rec.created),
      labels: { ...rec.labels },
      annotations: { ...rec.annotations },
      ...(ownerReferences && { ownerReferences }),
    },
    spec: taskSpec(rec),
    status,
    ...(plan && { plan }),
  }
}

function deliveryStatus(rec: TaskRecord, phase: TaskPhase): NonNullable<Task['status']>['delivery'] {
  const repo = rec.repo ? REPOS[rec.repo] : undefined
  const identity = repo ? { provider: 'github', id: repo.slug } : undefined
  if (!writeIntent(rec)) {
    if (!repo) return { state: 'NotRequested', outcome: 'NotRequested' }
    if (phase === 'Pending') return { state: 'NotRequested' }
    return phase === 'Succeeded'
      ? { state: 'ReadValidated', outcome: 'ReadValidated', sourceRepository: identity, lastTransitionTime: iso(rec.end) }
      : { state: 'Validating', sourceRepository: identity }
  }
  if (phase === 'Pending') return { state: 'NotRequested' }
  if (phase === 'Running') return { state: 'Preparing', sourceRepository: identity, publicationRepository: identity }
  const pr = rec.outcome.pr
  if (phase !== 'Succeeded' || !pr) {
    return { state: 'CancelledBeforePublish', outcome: 'CancelledBeforePublish', reason: `Task${phase}`, message: 'No changes were published', sourceRepository: identity, lastTransitionTime: iso(rec.end) }
  }
  const head = digest(`head:${rec.name}`, 40)
  return {
    state: 'VerifiedExact',
    outcome: 'VerifiedExact',
    publicationID: `pub-${digest(`publication:${rec.name}`, 12)}`,
    sourceRepository: identity,
    publicationRepository: identity,
    branch: pr.branch,
    startingSHA: digest(`start:${rec.name}`, 40),
    remoteBeforeSHA: null,
    treeSHA: digest(`tree:${rec.name}`, 40),
    expectedCommitSHA: head,
    verifiedRemoteSHA: head,
    artifactDigest: `sha256:${digest(`artifact:${rec.name}`, 64)}`,
    prReceipt: {
      id: `PR_kwDO${digest(`pr:${rec.name}`, 14)}`,
      number: pr.number,
      url: `${repo?.url}/pull/${pr.number}`,
      state: pr.status === 'merged' ? 'merged' : pr.status,
      baseBranch: 'main',
      headBranch: pr.branch,
      headSHA: head,
    },
    message: `Opened pull request #${pr.number}`,
    lastTransitionTime: iso(rec.end),
  }
}

function buildTrace(rec: TaskRecord, now = Date.now()): TaskTrace {
  const task = toTask(rec, now)
  const traceEvent = (e: ExecutionEvent) => ({
    seq: e.seq,
    type: e.type,
    severity: e.severity,
    summary: e.summary,
    taskName: e.taskName,
    agentName: e.agentName,
    toolName: e.toolName,
    toolCallID: e.toolCallID,
    content: e.content,
    contentText: e.contentText,
    createdAt: e.createdAt,
  })

  const modelRequests: TaskTrace['modelRequests'] = []
  let openModel: TaskTrace['modelRequests'][number] | undefined
  const toolCalls = new Map<string, TaskTrace['toolCalls'][number]>()
  const workspace: TaskTrace['workspace'] = []
  const artifacts: TaskTrace['artifacts'] = []
  const errors: TaskTrace['errors'] = []
  const warnings: TaskTrace['warnings'] = []

  for (const e of rec.events) {
    if (e.type === 'ModelRequestStarted') {
      openModel = { id: `model-${e.seq}`, status: 'running', model: e.model, provider: e.provider, startSeq: e.seq, startedAt: e.createdAt }
      modelRequests.push(openModel)
    } else if ((e.type === 'ModelRequestCompleted' || e.type === 'ModelRequestFailed') && openModel) {
      Object.assign(openModel, { status: e.type === 'ModelRequestFailed' ? 'failed' : 'completed', endSeq: e.seq, endedAt: e.createdAt, summary: e.summary })
      openModel = undefined
    } else if (e.type === 'ToolCallStarted' && e.toolCallID) {
      toolCalls.set(e.toolCallID, { id: e.toolCallID, name: e.toolName, status: 'running', startSeq: e.seq, startedAt: e.createdAt, summary: e.summary })
    } else if ((e.type === 'ToolCallCompleted' || e.type === 'ToolCallFailed') && e.toolCallID) {
      const call = toolCalls.get(e.toolCallID)
      if (call) Object.assign(call, { status: e.type === 'ToolCallFailed' ? 'failed' : 'completed', endSeq: e.seq, endedAt: e.createdAt, summary: e.summary, ...(e.type === 'ToolCallFailed' && { error: e.summary }) })
    } else if (e.type.startsWith('WorkspacePreparation')) {
      workspace.push({ status: e.type.endsWith('Started') ? 'running' : e.type.endsWith('Failed') ? 'failed' : 'completed', seq: e.seq, summary: e.summary, createdAt: e.createdAt })
    } else if (e.type.startsWith('ArtifactUpload')) {
      const content = e.content as { filename?: string } | undefined
      artifacts.push({ name: content?.filename, status: e.type.endsWith('Failed') ? 'failed' : 'completed', seq: e.seq, summary: e.summary, createdAt: e.createdAt })
    }
    if (e.severity === 'error') errors.push({ seq: e.seq, type: e.type, severity: e.severity, message: e.summary ?? e.type })
    if (e.severity === 'warning' && !e.type.startsWith('Approval') && e.type !== 'TaskCancelled') warnings.push({ seq: e.seq, type: e.type, severity: e.severity, message: e.summary ?? e.type })
  }

  const terminal = terminalEvent(rec)
  return {
    task: {
      namespace: DEMO_NAMESPACE,
      name: rec.name,
      type: rec.type,
      phase: task.status?.phase,
      agentName: agentNameOf(rec),
      sessionName: rec.session,
      resultAvailable: rec.finished && Boolean(rec.outcome.result),
    },
    latestSeq: rec.events.length,
    generatedAt: iso(now),
    timeline: rec.events.map(traceEvent),
    modelRequests,
    toolCalls: [...toolCalls.values()],
    childTasks: (task.status?.childTasks ?? []).map((child) => {
      const childRec = tasks.get(child.name)
      return {
        name: child.name,
        agent: child.agent,
        status: child.phase === 'Succeeded' ? 'completed' : child.phase === 'Failed' ? 'failed' : 'running',
        startedAt: childRec?.started ? iso(childRec.start) : undefined,
        endedAt: childRec?.finished ? iso(childRec.end) : undefined,
        summary: child.phase,
        result: child.result,
      }
    }),
    workspace,
    artifacts,
    errors,
    warnings,
    ...(terminal && { terminalEvent: traceEvent(terminal) }),
  }
}

// ---- sessions ---------------------------------------------------------------------

function ensureSession(name: string, agent: AgentKey | undefined, created: number) {
  if (!sessions.has(name)) sessions.set(name, { name, agent, created, messages: [] })
}

function sessionTasks(name: string) {
  return [...tasks.values()].filter((rec) => rec.session === name)
}

/** The session's event stream: its tasks' events merged in time order. */
function sessionEvents(name: string): ExecutionEvent[] {
  const merged = sessionTasks(name)
    .flatMap((rec) => rec.events)
    .sort((a, b) => a.createdAt.localeCompare(b.createdAt) || (a.taskName ?? '').localeCompare(b.taskName ?? '') || a.seq - b.seq)
  return merged.map((event, index) => ({
    ...event,
    streamType: 'session',
    streamID: name,
    seq: index + 1,
    sessionName: name,
    taskSeq: event.seq,
    taskStreamID: event.streamID,
  }))
}

function sessionListItem(rec: SessionRecord, now = Date.now()) {
  const owned = sessionTasks(rec.name)
  const active = owned.filter((task) => !TERMINAL.has(phaseOf(task, now))).sort((a, b) => b.created - a.created)[0]
  const sum = (key: 'inputTokens' | 'outputTokens') => rec.messages.reduce((total, message) => total + (message[key] ?? 0), 0)
  const lastMessage = rec.messages[rec.messages.length - 1]?.timestamp
  const lastEvent = owned.flatMap((task) => task.events.map((e) => e.createdAt)).sort().pop()
  const updatedAt = [lastMessage, lastEvent, iso(rec.created)].filter(Boolean).sort().pop()
  return {
    name: rec.name,
    namespace: DEMO_NAMESPACE,
    messageCount: String(rec.messages.length),
    inputTokens: String(sum('inputTokens')),
    outputTokens: String(sum('outputTokens')),
    ...(active && { activeTask: active.name }),
    createdAt: iso(rec.created),
    updatedAt,
  }
}

// ---- store setup ------------------------------------------------------------------

function seedStore() {
  for (const seed of TASKS) {
    if (!tasks.has(seed.name)) tasks.set(seed.name, seedRecord(seed))
  }
  for (const fixture of SESSION_FIXTURES) {
    sessions.set(fixture.name, {
      name: fixture.name,
      agent: fixture.agent,
      created: NOW - fixture.createdAgo * 60_000,
      messages: fixture.messages,
      acp: fixture.acp,
    })
  }
}

seedStore()

// ---- HTTP -------------------------------------------------------------------------

const STREAM_CYCLE_MS = 30_000

/**
 * A long-poll SSE stream of execution events: replays events after `after`,
 * follows new ones as the story plays, and ends with stream_complete once the
 * task is terminal (or after one cycle, after which the client reconnects).
 */
function eventStream(signal: AbortSignal, after: number, read: () => { events: ExecutionEvent[]; complete?: { lastSeq: number; type: string } } | undefined) {
  const encoder = new TextEncoder()
  let cursor = after
  let timer: ReturnType<typeof setTimeout> | undefined
  const deadline = Date.now() + STREAM_CYCLE_MS
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const tick = () => {
        try {
          advanceAll()
          const state = signal.aborted ? undefined : read()
          if (!state) {
            controller.close()
            return
          }
          for (const event of state.events) {
            if (event.seq <= cursor) continue
            controller.enqueue(encoder.encode(`id: ${event.seq}\n${sseFrame('execution_event', event)}`))
            cursor = event.seq
          }
          if (state.complete && cursor >= state.complete.lastSeq) {
            controller.enqueue(encoder.encode(sseFrame('stream_complete', state.complete)))
            controller.close()
            return
          }
          if (Date.now() >= deadline) {
            controller.close()
            return
          }
          timer = setTimeout(tick, 1000)
        } catch {
          // The reader went away.
        }
      }
      tick()
    },
    cancel() {
      if (timer) clearTimeout(timer)
    },
  })
  return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' } })
}

/** The task for a request, after playing its story up to now. */
function lookup(request: Request, name: string): TaskRecord | undefined {
  if (!inDemoNamespace(request)) return undefined
  advanceAll()
  return tasks.get(name)
}

const nameParam = (params: Record<string, string | readonly string[] | undefined>, key = 'id') => String(params[key] ?? '')

const forbiddenNamespace = () => apiError(403, `the demo only accepts writes in namespace "${DEMO_NAMESPACE}"`)

interface CreateTaskBody {
  name?: string
  namespace?: string
  type?: 'ai' | 'agent' | 'container'
  image?: string
  command?: string[]
  agentRef?: { name?: string }
  ai?: Task['spec']['ai']
  prompt?: string
  workspace?: Task['spec']['workspace']
  agentRuntime?: Task['spec']['agentRuntime']
  priority?: number
  timeout?: string
}

const agentKeyByName = (name?: string) => (Object.keys(AGENTS) as AgentKey[]).find((key) => AGENTS[key].name === name)
const repoKeyByUrl = (url?: string) => (Object.keys(REPOS) as RepoKey[]).find((key) => url?.replace(/\.git$/, '') === REPOS[key].url)

export const taskHandlers: RequestHandler[] = [
  http.get(api('/tasks'), ({ request }) => {
    if (!inDemoNamespace(request)) return list([])
    advanceAll()
    const now = Date.now()
    return list([...tasks.values()].sort((a, b) => a.name.localeCompare(b.name)).map((rec) => toTask(rec, now)))
  }),

  http.post(api('/tasks'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as CreateTaskBody
    if (body.namespace && body.namespace !== DEMO_NAMESPACE) return forbiddenNamespace()
    const type = body.type ?? 'container'
    const name = body.name?.trim() || `${type}-task-${randomSuffix()}`
    if (!isDNSLabel(name)) return apiError(400, `invalid task name "${name}": must be a lowercase RFC 1123 label`)
    if (tasks.has(name)) return apiError(409, `tasks.orka.ai "${name}" already exists`)
    const agentName = body.agentRef?.name
    const agent = agentKeyByName(agentName)
    const prompt = body.prompt ?? body.ai?.prompt ?? [body.image, ...(body.command ?? [])].filter(Boolean).join(' ')
    const record = addLiveTask({
      name,
      type,
      agent: type === 'container' ? undefined : agent ?? (type === 'ai' ? 'coordinator' : 'reviewer'),
      agentName: type === 'container' ? undefined : agentName,
      repo: repoKeyByUrl(body.workspace?.gitRepo),
      prompt,
      createdBy: 'demo-user',
      specExtras: {
        ...(body.image && { image: body.image }),
        ...(body.command && { command: body.command }),
        ...(body.ai && { ai: body.ai }),
        ...(body.workspace && { workspace: body.workspace }),
        ...(body.agentRuntime && { agentRuntime: body.agentRuntime }),
        ...(body.priority !== undefined && { priority: body.priority }),
        ...(body.timeout && { timeout: body.timeout }),
      },
    })
    advance(record, Date.now())
    return HttpResponse.json(toTask(record), { status: 201 })
  }),

  http.get(api('/tasks/:id'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    return rec ? HttpResponse.json(toTask(rec)) : notFound(`tasks.orka.ai "${nameParam(params)}"`)
  }),

  http.delete(api('/tasks/:id'), ({ request, params }) => {
    if (!inDemoNamespace(request)) return forbiddenNamespace()
    const name = nameParam(params)
    if (!tasks.delete(name)) return notFound(`tasks.orka.ai "${name}"`)
    return noContent()
  }),

  http.get(api('/tasks/:id/result'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    if (!rec) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    if (!rec.finished || !rec.outcome.result) return notFound('result')
    return HttpResponse.json({ result: rec.outcome.result })
  }),

  http.get(api('/tasks/:id/children'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    if (!rec) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    const now = Date.now()
    return list(children(rec.name).map((child) => toTask(child, now)))
  }),

  http.get(api('/tasks/:id/logs'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    if (!rec) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    if (rec.schedule) return apiError(404, 'a scheduled task has no pod of its own; each run has its own logs')
    if (rec.type === 'agent') return apiError(404, `this task runs on RuntimePool acp-${runtimeOf(rec)}, which has no per-task pod`)
    if (!rec.started) return apiError(404, 'the task\'s pod has not started yet')
    if (!rec.finished) {
      const tail = Number(query(request, 'tailLines') ?? 200)
      return HttpResponse.json({ logs: `${rec.logs.slice(-tail).join('\n')}\n` })
    }
    if (!rec.logs.length) return apiError(404, 'no logs were recorded for this task')
    return new HttpResponse(`${rec.logs.join('\n')}\n`, { headers: { 'Content-Type': 'text/plain; charset=utf-8' } })
  }),

  http.get(api('/tasks/:id/events'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    if (!rec) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    const after = Number(query(request, 'after') ?? 0)
    const limit = Math.min(Number(query(request, 'limit') ?? 100), 1000)
    return HttpResponse.json({
      namespace: DEMO_NAMESPACE,
      streamType: 'task',
      streamID: rec.name,
      afterSeq: after,
      latestSeq: rec.events.length,
      events: rec.events.filter((e) => e.seq > after).slice(0, limit),
    })
  }),

  http.get(api('/tasks/:id/stream'), ({ request, params }) => {
    const name = nameParam(params)
    if (!lookup(request, name)) return notFound(`tasks.orka.ai "${name}"`)
    return eventStream(request.signal, Number(query(request, 'after') ?? 0), () => {
      const rec = tasks.get(name)
      if (!rec) return undefined
      const terminal = rec.finished ? terminalEvent(rec) : undefined
      return { events: rec.events, complete: terminal && { lastSeq: rec.events[rec.events.length - 1].seq, type: terminal.type } }
    })
  }),

  http.get(api('/tasks/:id/trace'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    return rec ? HttpResponse.json(buildTrace(rec)) : notFound(`tasks.orka.ai "${nameParam(params)}"`)
  }),

  http.get(api('/tasks/:id/approvals'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    if (!rec) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    return HttpResponse.json({ namespace: DEMO_NAMESPACE, taskName: rec.name, approvals: rec.approvals })
  }),

  http.post(api('/tasks/:id/approvals/:approvalId/decision'), async ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    if (!rec) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    const approval = rec.approvals.find((a) => a.id === nameParam(params, 'approvalId'))
    if (!approval) return notFound('approval')
    const body = (await request.json().catch(() => ({}))) as { decision?: string; reason?: string }
    if (body.decision !== 'approve' && body.decision !== 'decline') return apiError(400, 'decision must be "approve" or "decline"')
    if (approval.status !== 'pending' || rec.finished) return apiError(409, `approval ${approval.id} is already ${approval.status}`)
    decide(rec, approval, body.decision, body.reason, Date.now())
    return HttpResponse.json(approval)
  }),

  http.post(api('/tasks/:id/fork'), async ({ request, params }) => {
    const source = lookup(request, nameParam(params))
    if (!source) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    const body = (await request.json().catch(() => ({}))) as ForkTaskRequest
    const afterSeq = Math.min(body.afterSeq ?? source.events.length, source.events.length)
    const name = body.newTaskName?.trim() || `${source.name.slice(0, 40)}-fork-${randomSuffix()}`
    if (!isDNSLabel(name)) return apiError(400, `invalid task name "${name}": must be a lowercase RFC 1123 label`)
    if (tasks.has(name)) return apiError(409, `tasks.orka.ai "${name}" already exists`)
    const now = Date.now()
    emit(source, { type: 'TaskForkRequested', summary: `Fork requested after seq ${afterSeq}`, content: { afterSeq } }, now)
    const agentName = body.agentRef?.name ?? agentNameOf(source)
    addLiveTask({
      name,
      type: source.type,
      agent: agentKeyByName(agentName) ?? source.agent,
      agentName,
      repo: source.repo,
      prompt: body.prompt?.trim() || source.prompt,
      createdBy: 'demo-user',
      parent: undefined,
      labels: { 'orka.ai/parent-task': source.name },
      annotations: { 'orka.ai/fork-source-task': source.name, 'orka.ai/fork-source-seq': String(afterSeq) },
      specExtras: {
        // A fork runs the source's container, not the catalog default.
        ...(source.type === 'container' && containerSpec(source)),
        ...(body.workspace && { workspace: body.workspace as Task['spec']['workspace'] }),
      },
    })
    emit(source, { type: 'TaskForkCreated', summary: `Forked into ${name}`, content: { newTaskName: name } }, now + 50)
    return HttpResponse.json({
      namespace: DEMO_NAMESPACE,
      sourceTaskName: source.name,
      newTaskName: name,
      afterSeq,
      forkContext: {
        sourceNamespace: DEMO_NAMESPACE,
        sourceTask: source.name,
        afterSeq,
        events: source.events.filter((e) => e.seq <= afterSeq).map((e) => ({
          seq: e.seq,
          type: e.type,
          severity: e.severity,
          summary: e.summary,
          toolName: e.toolName,
          toolCallID: e.toolCallID,
        })),
        truncated: false,
      },
    }, { status: 201 })
  }),

  http.get(api('/tasks/:id/artifacts'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    if (!rec) return notFound(`tasks.orka.ai "${nameParam(params)}"`)
    return HttpResponse.json({ artifacts: rec.artifacts.map((artifact) => ({ filename: artifact.filename, contentType: artifact.contentType, size: artifact.size, createdAt: artifact.createdAt })) })
  }),

  http.get(api('/tasks/:id/artifacts/:filename'), ({ request, params }) => {
    const rec = lookup(request, nameParam(params))
    const artifact = rec?.artifacts.find((a) => a.filename === nameParam(params, 'filename'))
    if (!artifact) return notFound('artifact')
    return new HttpResponse(artifact.body, { headers: { 'Content-Type': artifact.contentType ?? 'application/octet-stream' } })
  }),

  http.get(api('/sessions'), ({ request }) => {
    if (!inDemoNamespace(request)) return list([])
    advanceAll()
    const now = Date.now()
    return list([...sessions.values()].sort((a, b) => a.name.localeCompare(b.name)).map((rec) => sessionListItem(rec, now)))
  }),

  http.get(api('/sessions/:id'), ({ request, params }) => {
    if (!inDemoNamespace(request)) return notFound('session')
    advanceAll()
    const rec = sessions.get(nameParam(params))
    if (!rec) return notFound(`session "${nameParam(params)}"`)
    return HttpResponse.json({
      ...sessionListItem(rec),
      transcript: rec.messages.map((message) => JSON.stringify(message)).join('\n'),
      ...(rec.acp && { executionControl: sessionExecutionControl(rec.name, rec.acp, (NOW - rec.created) / 60_000) }),
    })
  }),

  http.delete(api('/sessions/:id'), ({ request, params }) => {
    if (!inDemoNamespace(request)) return forbiddenNamespace()
    if (!sessions.delete(nameParam(params))) return notFound(`session "${nameParam(params)}"`)
    return noContent()
  }),

  http.get(api('/sessions/:id/events'), ({ request, params }) => {
    if (!inDemoNamespace(request)) return notFound('session')
    advanceAll()
    const name = nameParam(params)
    if (!sessions.has(name)) return notFound(`session "${name}"`)
    const after = Number(query(request, 'after') ?? 0)
    const limit = Math.min(Number(query(request, 'limit') ?? 100), 1000)
    const events = sessionEvents(name)
    return HttpResponse.json({
      namespace: DEMO_NAMESPACE,
      streamType: 'session',
      streamID: name,
      afterSeq: after,
      latestSeq: events.length,
      events: events.filter((e) => e.seq > after).slice(0, limit),
    })
  }),

  http.get(api('/sessions/:id/stream'), ({ request, params }) => {
    const name = nameParam(params)
    if (!inDemoNamespace(request) || !sessions.has(name)) return notFound(`session "${name}"`)
    // Session streams stay open while the session exists.
    return eventStream(request.signal, Number(query(request, 'after') ?? 0), () => (sessions.has(name) ? { events: sessionEvents(name) } : undefined))
  }),
]
