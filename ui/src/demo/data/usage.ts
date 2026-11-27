import type { UsageMeasurement } from '@/lib/usage'
import { DEMO_NAMESPACE, NOW, digest, taskUID, uid } from '../shared'
import { AGENTS, REPOS, SESSIONS, TASKS, type AgentKey, type DemoTaskSeed, type RepoKey } from '../world'

export interface UsageRecordTask {
  namespace: string
  taskUID: string
  taskName?: string
  sessionName?: string
  workID?: string
  repository?: string
  prNumber?: number
  phase: string
  role?: string
  runtime?: string
  startedAt: string
  measurements: UsageMeasurement[]
}

export interface UsageWorkRecord {
  id: string
  namespace: string
  monitorName: string
  monitorUID: string
  repository: string
  kind: 'issue'
  number: number
  startedAt: string
}

export interface UsagePRRecord {
  namespace: string
  repository: string
  number: number
  url: string
  state: 'open' | 'merged' | 'closed'
  headSHA: string
  ready: boolean
  readinessReason?: string
  mergedAt?: string
  observedAt: string
}

export interface UsageLinkRecord {
  workID: string
  repository: string
  number: number
  origin: 'created' | 'assisted' | 'review_only'
  linkedAt: string
}

export interface UsageLedger {
  works: UsageWorkRecord[]
  tasks: UsageRecordTask[]
  pullRequests: UsagePRRecord[]
  links: UsageLinkRecord[]
  retainedSince: string
}

const MINUTE = 60_000
const at = (ms: number) => new Date(ms).toISOString()
export const pastTaskUID = (name: string) => uid(`task:${name}`)
const liveTaskUID = (seed: DemoTaskSeed) => taskUID(seed.name, NOW - seed.createdAgo * MINUTE)

function mulberry32(seed: number) {
  let state = seed >>> 0
  return () => {
    state = (state + 0x6d2b79f5) >>> 0
    let t = state
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

const suffix = (seed: string) => digest(seed, 10).replace(/[^a-z0-9]/g, '').slice(0, 5)

function workID(repository: string, number: number) {
  return `work-${digest(`${DEMO_NAMESPACE}/${repository}/issue/${number}`, 32)}`
}

const monitorName = (repo: RepoKey) => REPOS[repo].slug.split('/')[1]

function work(repo: RepoKey, number: number, startedAtMs: number): UsageWorkRecord {
  const name = monitorName(repo)
  return {
    id: workID(REPOS[repo].slug, number),
    namespace: DEMO_NAMESPACE,
    monitorName: name,
    monitorUID: uid(`repositorymonitor:${name}`),
    repository: REPOS[repo].slug,
    kind: 'issue',
    number,
    startedAt: at(startedAtMs),
  }
}

interface MeasureOptions {
  agent: AgentKey
  taskName: string
  input: number
  output: number
  startMs: number
  durationMin: number
  scope: 'call' | 'attempt'
  running?: boolean
  unreported?: boolean
  failed?: boolean
}

function measurements(options: MeasureOptions): UsageMeasurement[] {
  const { agent, taskName, input, output, startMs, durationMin, scope } = options
  const provider = AGENTS[agent].provider
  const model = AGENTS[agent].model
  const anthropic = provider === 'anthropic'
  const base = pastTaskUID(taskName)
  if (scope === 'attempt') {
    const cached = Math.round(input * 0.42)
    return [{
      id: `${base}/attempt-1`,
      attemptID: uid(`attempt:${taskName}`),
      scope: 'attempt',
      source: 'agent',
      provider,
      model,
      inputTokens: options.unreported ? null : input,
      outputTokens: options.unreported ? null : output,
      cachedInputTokens: options.unreported ? null : cached,
      cacheWriteInputTokens: options.unreported ? null : anthropic ? Math.round(input * 0.08) : 0,
      status: options.running ? 'started' : options.failed ? 'failed' : 'completed',
      completeness: options.unreported ? 'unavailable' : options.running ? 'partial' : 'complete',
      gap: options.unreported
        ? 'Attempt-level reporting only; no consumed-token counts reported'
        : options.running ? 'Attempt still running; counts reflect usage reported so far' : undefined,
      observedAt: at(startMs + Math.max(1, durationMin) * MINUTE),
    }]
  }
  const calls = Math.max(1, Math.min(9, Math.round(output / 600)))
  const weights = Array.from({ length: calls }, (_, i) => (i === 0 ? 1.6 : 1 + (i % 3) * 0.15))
  const total = weights.reduce((sum, w) => sum + w, 0)
  let inputLeft = input
  let outputLeft = output
  return weights.map((weight, i) => {
    const last = i === calls - 1
    const callInput = last ? inputLeft : Math.round((input * weight) / total)
    const callOutput = last ? outputLeft : Math.round((output * weight) / total)
    inputLeft -= callInput
    outputLeft -= callOutput
    return {
      id: `${base}/call-${String(i + 1).padStart(2, '0')}`,
      scope: 'call',
      source: 'provider',
      provider,
      model,
      inputTokens: callInput,
      outputTokens: callOutput,
      cachedInputTokens: i === 0 ? 0 : Math.round(callInput * 0.55),
      cacheWriteInputTokens: anthropic && i === 0 ? Math.round(callInput * 0.6) : 0,
      status: 'completed',
      completeness: 'complete',
      observedAt: at(startMs + ((i + 1) / calls) * Math.max(0.5, durationMin) * MINUTE),
    }
  })
}

const ROLE_BY_AGENT: Record<AgentKey, string> = {
  triage: 'triage',
  reviewer: 'review',
  coordinator: 'coordinate',
  incident: 'incident',
  fixer: 'implement',
  deps: 'dependencies',
  docs: 'docs',
  security: 'security',
}

function currentTaskRecord(seed: DemoTaskSeed, workIDs: Map<string, string>): UsageRecordTask | undefined {
  if (!seed.agent || seed.phase === 'Scheduled' || seed.phase === 'Pending' || seed.inputTokens === undefined) return undefined
  const startMs = NOW - seed.createdAgo * MINUTE
  const running = seed.phase === 'Running'
  const durationMin = running ? Math.max(1, seed.createdAgo - 0.5) : seed.durationMin ?? 1
  const prMatch = /^review-pr-(\d+)-/.exec(seed.name)
  return {
    namespace: DEMO_NAMESPACE,
    taskUID: liveTaskUID(seed),
    taskName: seed.name,
    sessionName: seed.session,
    workID: workIDs.get(seed.name),
    repository: seed.repo ? REPOS[seed.repo].slug : undefined,
    prNumber: prMatch ? Number(prMatch[1]) : undefined,
    phase: seed.phase,
    role: ROLE_BY_AGENT[seed.agent],
    runtime: seed.type,
    startedAt: at(startMs),
    measurements: measurements({
      agent: seed.agent,
      taskName: seed.name,
      input: seed.inputTokens,
      output: seed.outputTokens ?? 0,
      startMs,
      durationMin,
      scope: seed.type === 'agent' ? 'attempt' : 'call',
      running,
      failed: seed.phase === 'Failed',
    }),
  }
}

const REPO_ROTATION: RepoKey[] = ['checkout', 'inventory', 'storefront', 'checkout', 'infra', 'inventory', 'storefront']
const FIRST_ISSUE: Record<RepoKey, number> = { checkout: 2121, inventory: 3290, storefront: 791, infra: 402 }
const NOT_READY_REASONS = [
  "Required check 'integration-tests' is failing",
  'Awaiting 1 approving review',
  'Merge conflict with main',
]

function generateHistory(ledger: UsageLedger) {
  const random = mulberry32(0x0a17)
  const pick = <T,>(items: readonly T[]) => items[Math.floor(random() * items.length)]
  const taken = new Map<RepoKey, Set<number>>()
  const claim = (repo: RepoKey, from: number) => {
    const used = taken.get(repo) ?? new Set<number>()
    taken.set(repo, used)
    let number = from
    while (used.has(number)) number++
    used.add(number)
    return number
  }
  const nextIssue = { ...FIRST_ISSUE }
  const days = 42
  const count = Math.round(days * 1.7)
  for (let i = 0; i < count; i++) {
    const repo = REPO_ROTATION[i % REPO_ROTATION.length]
    const number = (nextIssue[repo] = claim(repo, nextIssue[repo] + 1 + Math.floor(random() * 4)))
    const ageMin = ((count - i) / count) * days * 24 * 60 + 300 + random() * 240
    const startMs = NOW - ageMin * MINUTE
    const request = work(repo, number, startMs)
    ledger.works.push(request)
    const add = (name: string, agent: AgentKey, phase: string, offsetMin: number, durationMin: number, input: number, output: number, extra: Partial<MeasureOptions> & { prNumber?: number } = {}) => {
      const begin = startMs + offsetMin * MINUTE
      ledger.tasks.push({
        namespace: DEMO_NAMESPACE,
        taskUID: pastTaskUID(name),
        taskName: name,
        workID: request.id,
        repository: request.repository,
        prNumber: extra.prNumber,
        phase,
        role: name.startsWith('repair') ? 'repair' : ROLE_BY_AGENT[agent],
        runtime: AGENTS[agent].kind,
        startedAt: at(begin),
        measurements: measurements({
          agent, taskName: name, input, output, startMs: begin, durationMin,
          scope: AGENTS[agent].kind === 'agent' ? 'attempt' : 'call',
          failed: phase === 'Failed',
          unreported: extra.unreported,
        }),
      })
      return begin + durationMin * MINUTE
    }

    add(`triage-issue-${number}-${suffix(`t${i}`)}`, 'triage', 'Succeeded', 0.4, 1, 6000 + Math.round(random() * 4000), 650 + Math.round(random() * 500))
    const implementer: AgentKey = repo === 'infra' && random() < 0.5 ? 'docs' : 'fixer'
    const roll = random()
    const failed = roll < 0.1
    let implemented = add(
      `fix-issue-${number}-${suffix(`f${i}`)}`, implementer, failed ? 'Failed' : 'Succeeded',
      3, 6 + random() * 14, 70_000 + Math.round(random() * 90_000), 5_000 + Math.round(random() * 8_000),
      { unreported: implementer === 'docs' && ageMin > 20 * 24 * 60 },
    )
    if (failed) {
      const retryAfterMin = (9 + random() * 6) * 24 * 60
      if (ageMin < retryAfterMin + 120) continue
      implemented = add(
        `fix-issue-${number}-${suffix(`x${i}`)}`, 'fixer', 'Succeeded',
        retryAfterMin, 6 + random() * 12, 80_000 + Math.round(random() * 60_000), 6_000 + Math.round(random() * 5_000),
      )
    }

    const pr = claim(repo, number + 2 + Math.floor(random() * 3))
    const recent = NOW - implemented < 36 * 60 * MINUTE
    const outcome = recent
      ? (random() < 0.55 ? 'ready' : 'waiting')
      : roll < 0.68 ? 'merged' : roll < 0.8 ? 'ready' : roll < 0.9 ? 'waiting' : 'closed'
    const reviewed = random() < 0.75
    let doneMs = implemented
    if (reviewed) {
      doneMs = add(`review-pr-${pr}-${suffix(`r${i}`)}`, 'reviewer', 'Succeeded', (implemented - startMs) / MINUTE + 4, 3 + random() * 5, 38_000 + Math.round(random() * 50_000), 2_500 + Math.round(random() * 3_000), { prNumber: pr })
      if (random() < 0.35) {
        doneMs = add(`repair-pr-${pr}-${suffix(`p${i}`)}`, 'fixer', 'Succeeded', (doneMs - startMs) / MINUTE + 20, 4 + random() * 8, 40_000 + Math.round(random() * 40_000), 3_000 + Math.round(random() * 4_000), { prNumber: pr })
      }
    }
    const mergedMs = doneMs + (90 + random() * 1_400) * MINUTE
    const merged = outcome === 'merged' && mergedMs < NOW
    ledger.pullRequests.push({
      namespace: DEMO_NAMESPACE,
      repository: request.repository,
      number: pr,
      url: `${REPOS[repo].url}/pull/${pr}`,
      state: merged ? 'merged' : outcome === 'closed' ? 'closed' : 'open',
      headSHA: digest(`pr:${request.repository}#${pr}`, 40),
      ready: !merged && outcome === 'ready',
      readinessReason: !merged && outcome === 'waiting' ? pick(NOT_READY_REASONS) : undefined,
      mergedAt: merged ? at(mergedMs) : undefined,
      observedAt: at(Math.min(NOW - 6 * MINUTE, merged ? mergedMs + 2 * MINUTE : NOW - (8 + random() * 40) * MINUTE)),
    })
    ledger.links.push({ workID: request.id, repository: request.repository, number: pr, origin: 'created', linkedAt: at(implemented) })
    if (random() < 0.12) {
      const assisted = claim(repo, pr + 1)
      ledger.pullRequests.push({
        namespace: DEMO_NAMESPACE, repository: request.repository, number: assisted, url: `${REPOS[repo].url}/pull/${assisted}`,
        state: merged ? 'merged' : 'open', headSHA: digest(`pr:${request.repository}#${assisted}`, 40), ready: false,
        mergedAt: merged ? at(mergedMs + 30 * MINUTE) : undefined, observedAt: at(NOW - 15 * MINUTE),
      })
      ledger.links.push({ workID: request.id, repository: request.repository, number: assisted, origin: 'assisted', linkedAt: at(doneMs + 60 * MINUTE) })
    }
  }
}

const CURRENT_WORK: Array<{ task: string; repo: RepoKey; issue: number }> = [
  { task: 'fix-cart-rounding-bug-x9m4d', repo: 'checkout', issue: 2214 },
  { task: 'triage-issue-3391-m2x7c', repo: 'inventory', issue: 3391 },
  { task: 'triage-issue-3388-k4v1s', repo: 'inventory', issue: 3388 },
  { task: 'triage-issue-2216-w9d3e', repo: 'checkout', issue: 2216 },
]

function buildLedger(): UsageLedger {
  const ledger: UsageLedger = { works: [], tasks: [], pullRequests: [], links: [], retainedSince: at(NOW - 90 * 24 * 60 * MINUTE) }
  generateHistory(ledger)

  const workIDs = new Map<string, string>()
  for (const current of CURRENT_WORK) {
    const seed = TASKS.find((task) => task.name === current.task)
    if (!seed) continue
    const request = work(current.repo, current.issue, NOW - (seed.createdAgo + 0.6) * MINUTE)
    ledger.works.push(request)
    workIDs.set(current.task, request.id)
  }

  const outbox = work('checkout', 2201, NOW - 26 * 60 * MINUTE)
  ledger.works.push(outbox)
  const outboxFix = 'fix-issue-2201-c4r8t'
  ledger.tasks.push({
    namespace: DEMO_NAMESPACE, taskUID: pastTaskUID(outboxFix), taskName: outboxFix, workID: outbox.id,
    repository: outbox.repository, phase: 'Succeeded', role: 'implement', runtime: 'agent', startedAt: at(NOW - 25.8 * 60 * MINUTE),
    measurements: measurements({ agent: 'fixer', taskName: outboxFix, input: 132_600, output: 10_400, startMs: NOW - 25.8 * 60 * MINUTE, durationMin: 17, scope: 'attempt' }),
  })
  ledger.pullRequests.push({
    namespace: DEMO_NAMESPACE, repository: outbox.repository, number: 1479, url: `${REPOS.checkout.url}/pull/1479`, state: 'open',
    headSHA: digest('pr:checkout#1479', 40), ready: true, observedAt: at(NOW - 9 * MINUTE),
  })
  ledger.links.push({ workID: outbox.id, repository: outbox.repository, number: 1479, origin: 'created', linkedAt: at(NOW - 25.5 * 60 * MINUTE) })

  for (const seed of TASKS) {
    const record = currentTaskRecord(seed, workIDs)
    if (record) ledger.tasks.push(record)
  }

  for (const [index, session] of [SESSIONS.chatMaya, SESSIONS.releaseChat].entries()) {
    const startMs = NOW - (index === 0 ? 37 : 31) * MINUTE
    ledger.tasks.push({
      namespace: DEMO_NAMESPACE,
      taskUID: uid(`chat-usage:${session.name}`),
      sessionName: session.name,
      phase: 'Unknown',
      role: 'unassociated',
      startedAt: at(startMs),
      measurements: measurements({ agent: session.agent, taskName: session.name, input: 18_400 + index * 9_100, output: 1_650 + index * 820, startMs, durationMin: 2, scope: 'call' }),
    })
  }
  return ledger
}

export const usageLedger = buildLedger()
