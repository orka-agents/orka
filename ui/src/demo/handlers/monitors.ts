import { http, HttpResponse, type RequestHandler } from 'msw'
import type { MonitorCommand, MonitorItem, MonitorWorkAction, RepositoryMonitor } from '@/schemas/monitor'
import {
  actions,
  commands,
  implementationJobs,
  items,
  monitors,
  mutations,
  runs,
  viewMonitorRun,
  workActions,
  type MonitorRunRecord,
} from '../data/monitors'
import { api, apiError, DEMO_NAMESPACE, digest, inDemoNamespace, list, notFound, now, query, uid } from '../shared'
import { REPOS, type AgentKey, type RepoKey } from '../world'
import { cancelDemoTask, createDemoTask } from './tasks'

// Repository monitor API. Runs, commands, and the work they queue progress on
// timers that are settled lazily on the next request.

const PR_INTENTS = new Set(['review', 'fix', 'fix_ci', 'update_branch'])
const ISSUE_INTENTS = new Set(['triage', 'research', 'plan', 'implement', 'decompose', 'stop', 'resume'])

/** Which agent handles a command intent, and the issue phase it queues. */
const INTENT_AGENT: Record<string, AgentKey> = {
  review: 'reviewer', fix: 'fixer', fix_ci: 'fixer',
  triage: 'triage', research: 'coordinator', plan: 'coordinator', decompose: 'coordinator', implement: 'fixer',
}
const QUEUED_PHASE: Record<string, string> = {
  triage: 'triage_queued', research: 'research_queued', plan: 'plan_queued', decompose: 'plan_queued', implement: 'implementation_queued',
}
const DONE_PHASE: Record<string, string> = {
  triage: 'triaged', research: 'researched', plan: 'plan_ready', decompose: 'plan_ready', implement: 'patch_ready',
}

/** Timers for work queued from the dashboard. */
interface Pending {
  commandID: string
  commandProcessedMs: number
  workActionID?: string
  runningMs: number
  doneMs: number
  itemKey: string
  monitorName: string
  intent: string
}
const pending: Pending[] = []

/** Monitors created in the demo report Pending until this time. */
const readyAt = new Map<string, number>()

const findMonitor = (name: string) => monitors.find((m) => m.metadata.name === name)
const findItem = (monitorName: string, key: string) => items.find((i) => i.monitorName === monitorName && i.itemKey === key)

function settle(nowMs = Date.now()) {
  for (const entry of [...pending]) {
    const command = commands.find((c) => c.id === entry.commandID)
    if (command && command.status === 'accepted' && nowMs >= entry.commandProcessedMs) {
      command.status = 'processed'
      command.processedAt = new Date(entry.commandProcessedMs).toISOString()
    }
    if (!entry.workActionID) {
      // Stop and resume only flip the command's status.
      if (nowMs >= entry.commandProcessedMs) pending.splice(pending.indexOf(entry), 1)
      continue
    }
    const work = workActions.find((w) => w.id === entry.workActionID)
    if (work && work.status === 'queued' && nowMs >= entry.runningMs) {
      work.status = 'running'
      work.phase = entry.intent === 'review' ? 'reviewing' : entry.intent === 'implement' ? 'implementing' : 'running'
      work.updatedAt = new Date(entry.runningMs).toISOString()
    }
    if (nowMs < entry.doneMs) continue
    pending.splice(pending.indexOf(entry), 1)
    const doneAt = new Date(entry.doneMs).toISOString()
    if (work) {
      work.status = 'succeeded'
      work.phase = entry.intent === 'review' ? 'published' : 'completed'
      work.updatedAt = doneAt
      work.completedAt = doneAt
      const job = implementationJobs.find((j) => j.workActionID === work.id)
      if (job) {
        // A ready patch still waits for its pull request, so the job stays active.
        Object.assign(job, { phase: 'patch_ready', validationState: 'passed', patchArtifactID: `art_${digest(`${job.id}:patch`, 8)}`, updatedAt: doneAt })
      }
    }
    completeItem(entry, doneAt, work)
  }
}

/** Implementation job phases the controller still treats as in flight. */
const ACTIVE_JOB_PHASES = new Set(['implementation_queued', 'implementing', 'patch_ready', 'mutation_queued', 'mutating_to_pr'])

/**
 * Stop fences the item's in-flight work the way the controller does: queued and
 * running work actions, active implementation jobs, and the work's tasks are
 * cancelled, so work that would have finished later cannot undo the stop.
 */
function cancelItemWork(monitorName: string, kind: string, number: number, nowMs: number) {
  const at = new Date(nowMs).toISOString()
  const reason = 'stopped by command'
  for (const entry of [...pending]) {
    if (entry.monitorName === monitorName && entry.itemKey === `${kind}/${number}` && entry.workActionID) pending.splice(pending.indexOf(entry), 1)
  }
  for (const work of workActions) {
    if (work.monitorName !== monitorName || work.targetKind !== kind || work.targetNumber !== number) continue
    if (work.status !== 'queued' && work.status !== 'running') continue
    Object.assign(work, { status: 'cancelled', phase: 'cancelled', updatedAt: at, completedAt: at })
    if (work.taskName) cancelDemoTask(work.taskName, `Cancelled: repository monitor ${reason}`)
  }
  if (kind !== 'issue') return
  for (const job of implementationJobs) {
    if (job.monitorName !== monitorName || job.issueNumber !== number || !ACTIVE_JOB_PHASES.has(job.phase ?? '')) continue
    Object.assign(job, { phase: 'cancelled', error: reason, updatedAt: at, completedAt: at })
  }
}

/** Applies the outcome of finished work to the item and records an action. */
function completeItem(entry: Pending, doneAt: string, work?: MonitorWorkAction) {
  const item = findItem(entry.monitorName, entry.itemKey)
  if (!item) return
  item.updatedAt = doneAt
  let summary = ''
  if (item.kind === 'pull_request') {
    if (entry.intent === 'review') {
      item.lastVerdict = 'passed'
      summary = 'No blocking issues found; two suggestions posted inline.'
    } else if (entry.intent === 'update_branch') {
      summary = `Merged main into ${item.headBranch ?? 'the head branch'}.`
    } else {
      item.repairState = 'succeeded'
      item.ciState = 'pending'
      summary = 'Pushed a repair commit; CI is re-running.'
    }
  } else {
    item.workflowPhase = DONE_PHASE[entry.intent] ?? item.workflowPhase
    summary = `${entry.intent.replace(/_/g, ' ')} finished for issue #${item.number}.`
  }
  actions.unshift({
    id: `act_${entry.monitorName.split('-')[0]}_${digest(`${entry.commandID}:action`, 6)}`,
    monitorNamespace: DEMO_NAMESPACE,
    monitorName: entry.monitorName,
    kind: item.kind,
    number: item.number,
    actionKind: entry.intent,
    headSHA: item.headSHA,
    taskName: work?.taskName,
    commandEventID: entry.commandID,
    workActionID: work?.id,
    verdict: item.kind === 'pull_request' && entry.intent === 'review' ? 'passed' : 'success',
    confidence: 'high',
    summary,
    createdAt: doneAt,
  })
}

/** A monitor with run bookkeeping and readiness derived from the current state. */
function viewMonitor(monitor: RepositoryMonitor): RepositoryMonitor {
  const name = monitor.metadata.name
  const monitorRuns = runs.filter((r) => r.run.monitorName === name).sort((a, b) => b.startedMs - a.startedMs).map((r) => viewMonitorRun(r))
  const lastSucceeded = monitorRuns.find((r) => r.phase === 'succeeded')
  const pendingUntil = readyAt.get(name)
  const phase = pendingUntil && Date.now() < pendingUntil ? 'Pending' : monitor.status?.phase === 'Pending' ? 'Ready' : monitor.status?.phase
  // Queue counters follow the recorded items so dashboard commands move them.
  const prs = items.filter((i) => i.monitorName === name && i.kind === 'pull_request')
  const issues = items.filter((i) => i.monitorName === name && i.kind === 'issue')
  const counters = prs.length === 0 ? {} : {
    openPullRequests: prs.length,
    pendingReviews: prs.filter((i) => i.lastVerdict === 'queued' || i.lastVerdict === 'unseen').length,
    activeRepairs: prs.filter((i) => i.repairState === 'queued' || i.repairState === 'running').length,
    mergeReadyItems: prs.filter((i) => i.automergeState === 'merge_ready').length,
    ...(issues.length > 0 && { pendingIssueActions: issues.filter((i) => i.workflowPhase?.endsWith('_queued')).length }),
  }
  return {
    ...monitor,
    status: {
      ...monitor.status,
      ...counters,
      phase,
      lastRunID: monitorRuns[0]?.id,
      lastRunTime: monitorRuns[0]?.startedAt,
      lastSuccessfulRunTime: lastSucceeded?.completedAt,
      conditions: phase === 'Pending'
        ? [{ type: 'Ready', status: 'False', reason: 'Initializing', message: 'Waiting for the first GitHub inventory sync', lastTransitionTime: monitor.metadata.creationTimestamp }]
        : monitor.status?.conditions,
    },
  }
}

function startRun(monitorName: string, trigger: string, target?: Partial<MonitorRunRecord['run']>): MonitorRunRecord {
  const prefix = monitorName.split('-')[0]
  const seq = runs.filter((r) => r.run.monitorName === monitorName).reduce((max, r) => Math.max(max, Number(r.run.id.split('_').pop()) || 0), 0) + 1
  const startedMs = Date.now()
  const prs = items.filter((i) => i.monitorName === monitorName && i.kind === 'pull_request').length
  const record: MonitorRunRecord = {
    run: {
      id: `run_${prefix}_${String(seq).padStart(4, '0')}`,
      monitorNamespace: DEMO_NAMESPACE,
      monitorName,
      trigger,
      phase: 'running',
      startedAt: new Date(startedMs).toISOString(),
      ...target,
    },
    startedMs,
    finishMs: startedMs + 12_000,
    result: { phase: 'succeeded', selectedCount: prs, createdTaskCount: 0, skippedCount: prs },
  }
  runs.push(record)
  return record
}

const repoKeyFor = (monitor: RepositoryMonitor): RepoKey | undefined =>
  (Object.keys(REPOS) as RepoKey[]).find((key) => REPOS[key].url === monitor.spec.repoURL)

function taskPrompt(intent: string, item: MonitorItem | undefined, kind: string, number: number): string {
  const subject = kind === 'pull_request' ? `PR #${number}` : `issue #${number}`
  const title = item?.title ? ` "${item.title}"` : ''
  switch (intent) {
    case 'review': return `Review ${subject}${title}. Leave inline comments for anything blocking.`
    case 'fix': return `Address the review feedback on ${subject}${title} and push a fix commit.`
    case 'fix_ci': return `CI is failing on ${subject}${title}. Find the cause and push a fix.`
    case 'triage': return `Triage ${subject}${title}: label it, check for duplicates, and route it.`
    case 'research': return `Research ${subject}${title}: find related code, prior issues, and likely causes.`
    case 'plan': return `Write an implementation plan for ${subject}${title}.`
    case 'decompose': return `Break ${subject}${title} into independently shippable sub-issues.`
    default: return `Implement ${subject}${title} and open a pull request.`
  }
}

/** Starts a demo task for the command, if the tasks domain can create one. */
function startTask(intent: string, monitor: RepositoryMonitor, item: MonitorItem | undefined, kind: string, number: number): string | undefined {
  const agent = INTENT_AGENT[intent]
  if (!agent) return undefined
  try {
    return createDemoTask({ agent, prompt: taskPrompt(intent, item, kind, number), repo: repoKeyFor(monitor), createdBy: 'monitor' })
  } catch {
    return undefined
  }
}

async function readJSON<T>(request: Request): Promise<T | undefined> {
  try {
    return (await request.json()) as T
  } catch {
    return undefined
  }
}

const byMonitor = <T extends { monitorName: string }>(collection: T[], request: Request) => {
  settle()
  if (!inDemoNamespace(request)) return list([])
  const name = query(request, 'name')
  return list(collection.filter((entry) => !name || entry.monitorName === name))
}

/** Repository monitors: inventory, runs, commands, and the audit trail. */
export const monitorHandlers: RequestHandler[] = [
  http.get(api('/monitors/repositories'), ({ request }) => {
    settle()
    return list(inDemoNamespace(request) ? monitors.map(viewMonitor) : [])
  }),

  http.post(api('/monitors/repositories'), async ({ request }) => {
    const body = await readJSON<{ name?: string; namespace?: string; metadata?: { name?: string; namespace?: string }; spec?: RepositoryMonitor['spec'] }>(request)
    const name = (body?.name ?? body?.metadata?.name)?.trim()
    const namespace = body?.namespace ?? body?.metadata?.namespace ?? DEMO_NAMESPACE
    if (!body || !name || !body.spec?.repoURL) return apiError(400, 'name and spec.repoURL are required')
    if (namespace !== DEMO_NAMESPACE) return apiError(403, `the demo only accepts monitors in namespace ${DEMO_NAMESPACE}`)
    if (findMonitor(name)) return apiError(409, `repositorymonitors.orka.ai "${name}" already exists`)
    const monitor: RepositoryMonitor = {
      apiVersion: 'core.orka.ai/v1alpha1',
      kind: 'RepositoryMonitor',
      metadata: { name, namespace, uid: uid(`repositorymonitor:${name}:${Date.now()}`), creationTimestamp: now() },
      spec: body.spec,
      status: { phase: 'Pending', observedGeneration: 1, openPullRequests: 0, pendingReviews: 0, activeRepairs: 0, blockedItems: 0, mergeReadyItems: 0 },
    }
    monitors.push(monitor)
    readyAt.set(name, Date.now() + 5_000)
    startRun(name, 'initial')
    return HttpResponse.json(viewMonitor(monitor), { status: 201 })
  }),

  http.get(api('/monitors/repositories/:name'), ({ request, params }) => {
    settle()
    const monitor = inDemoNamespace(request) ? findMonitor(String(params.name)) : undefined
    return monitor ? HttpResponse.json(viewMonitor(monitor)) : notFound(`repository monitor ${params.name}`)
  }),

  http.get(api('/monitors/repositories/:name/runs'), ({ request, params }) => {
    if (!inDemoNamespace(request)) return list([])
    return list(runs.filter((r) => r.run.monitorName === params.name).sort((a, b) => b.startedMs - a.startedMs).map((r) => viewMonitorRun(r)))
  }),

  http.post(api('/monitors/repositories/:name/runs'), ({ request, params }) => {
    const monitor = inDemoNamespace(request) ? findMonitor(String(params.name)) : undefined
    if (!monitor) return notFound(`repository monitor ${params.name}`)
    if (monitor.spec.suspend) return apiError(409, 'repository monitor is suspended')
    if (runs.some((r) => r.run.monitorName === params.name && viewMonitorRun(r).phase === 'running')) {
      return apiError(409, 'a run is already in progress for this monitor')
    }
    return HttpResponse.json(viewMonitorRun(startRun(monitor.metadata.name, 'manual')), { status: 201 })
  }),

  http.get(api('/monitors/repositories/:name/items'), ({ request, params }) => {
    settle()
    if (!inDemoNamespace(request)) return list([])
    const kind = query(request, 'kind') ?? 'pull_request'
    return list(items.filter((i) => i.monitorName === params.name && i.kind === kind).sort((a, b) => (b.number ?? 0) - (a.number ?? 0)))
  }),

  http.post(api('/monitors/repositories/:name/commands'), async ({ request, params }) => {
    const monitor = inDemoNamespace(request) ? findMonitor(String(params.name)) : undefined
    if (!monitor) return notFound(`repository monitor ${params.name}`)
    const body = await readJSON<{ kind?: string; number?: number; intent?: string; targetSHA?: string }>(request)
    const kind = body?.kind
    const intent = body?.intent ?? ''
    const number = Number(body?.number)
    if ((kind !== 'pull_request' && kind !== 'issue') || !Number.isInteger(number) || number <= 0) {
      return apiError(400, 'kind must be pull_request or issue, with a positive number')
    }
    if (!(kind === 'pull_request' ? PR_INTENTS : ISSUE_INTENTS).has(intent)) return apiError(400, `intent "${intent}" is not valid for a ${kind.replace('_', ' ')}`)
    if (monitor.spec.suspend) return apiError(409, 'repository monitor is suspended')

    settle()
    const monitorName = monitor.metadata.name
    const prefix = monitorName.split('-')[0]
    const item = findItem(monitorName, `${kind}/${number}`)
    if (kind === 'pull_request' && body?.targetSHA && item?.headSHA && body.targetSHA !== item.headSHA) {
      return apiError(409, 'targetSHA does not match the current pull request head')
    }
    const createdMs = Date.now()
    const command: MonitorCommand = {
      id: `cmd_${prefix}_${digest(`${monitorName}:${kind}:${number}:${intent}:${createdMs}`, 6)}`,
      monitorNamespace: DEMO_NAMESPACE,
      monitorName,
      repo: `${monitor.spec.owner}/${monitor.spec.repository}`,
      kind,
      number,
      source: 'api',
      author: 'demo-user',
      permission: 'admin',
      intent,
      headSHA: body?.targetSHA || undefined,
      monitorGeneration: 1,
      dedupeKey: `${monitorName}:${kind}:${number}:${intent}`,
      idempotencyKey: digest(`${monitorName}:${kind}:${number}:${intent}:${createdMs}:idem`, 16),
      status: 'accepted',
      createdAt: new Date(createdMs).toISOString(),
    }
    commands.unshift(command)

    if (item) {
      item.lastCommandID = command.id
      item.lastCommandIntent = intent
      item.updatedAt = command.createdAt
    }

    if (intent === 'stop') cancelItemWork(monitorName, kind, number, createdMs)
    if (intent === 'stop' || intent === 'resume') {
      if (item) {
        item.workflowPhase = intent === 'stop' ? 'paused' : item.lastActionKind ? DONE_PHASE[item.lastActionKind] ?? 'triaged' : 'triage_queued'
        item.skipReason = intent === 'stop' ? 'stopped_by_command' : undefined
      }
      pending.push({ commandID: command.id, commandProcessedMs: createdMs + 2_000, runningMs: createdMs, doneMs: createdMs + 2_000, itemKey: `${kind}/${number}`, monitorName, intent })
      return HttpResponse.json(command, { status: 201 })
    }

    const taskName = startTask(intent, monitor, item, kind, number)
    const run = startRun(monitorName, 'command', { targetKind: kind, targetNumber: number, targetSHA: body?.targetSHA || undefined, commandEventID: command.id })
    run.result = { phase: 'succeeded', selectedCount: 1, createdTaskCount: taskName ? 1 : 0, skippedCount: 0 }
    const work: MonitorWorkAction = {
      id: `wa_${prefix}_${digest(`${command.id}:work`, 6)}`,
      monitorNamespace: DEMO_NAMESPACE,
      monitorName,
      runID: run.run.id,
      commandEventID: command.id,
      monitorGeneration: 1,
      targetKind: kind,
      targetNumber: number,
      targetSHA: body?.targetSHA || undefined,
      intent,
      desiredAction: intent === 'fix_ci' || intent === 'fix' ? 'repair' : intent,
      status: 'queued',
      phase: 'queued',
      attempt: 1,
      taskName,
      idempotencyKey: command.idempotencyKey,
      createdAt: command.createdAt,
      updatedAt: command.createdAt,
    }
    workActions.unshift(work)

    if (item) {
      if (kind === 'pull_request') {
        if (intent === 'review') item.lastVerdict = 'queued'
        else if (intent !== 'update_branch') item.repairState = 'queued'
      } else {
        item.workflowPhase = QUEUED_PHASE[intent] ?? item.workflowPhase
        item.skipReason = undefined
      }
      if (taskName) {
        item.lastActionKind = intent
        item.lastActionTaskName = taskName
      }
    }

    if (intent === 'implement') {
      implementationJobs.unshift({
        id: `impl_${prefix}_${digest(`${command.id}:impl`, 6)}`,
        monitorNamespace: DEMO_NAMESPACE,
        monitorName,
        repo: command.repo,
        issueNumber: number,
        phase: 'implementing',
        attempt: 1,
        branch: `orka/issue-${number}`,
        validationState: 'pending',
        taskName,
        commandEventID: command.id,
        workActionID: work.id,
        monitorGeneration: 1,
        createdAt: command.createdAt,
        updatedAt: command.createdAt,
      })
    }
    if (intent === 'update_branch') {
      mutations.unshift({
        id: `mut_${prefix}_${digest(`${command.id}:mutation`, 6)}`,
        monitorNamespace: DEMO_NAMESPACE,
        monitorName,
        runID: run.run.id,
        commandEventID: command.id,
        workActionID: work.id,
        monitorGeneration: 1,
        operation: 'update_branch',
        targetKind: kind,
        targetNumber: number,
        targetSHA: body?.targetSHA || undefined,
        actor: 'northwind-orka[bot]',
        reason: 'update_branch requested from the dashboard',
        status: 'succeeded',
        githubURL: `${monitor.spec.repoURL}/pull/${number}`,
        createdAt: command.createdAt,
      })
    }

    pending.push({
      commandID: command.id,
      commandProcessedMs: createdMs + 3_000,
      workActionID: work.id,
      runningMs: createdMs + 4_000,
      doneMs: createdMs + (intent === 'update_branch' ? 8_000 : 45_000),
      itemKey: `${kind}/${number}`,
      monitorName,
      intent,
    })
    return HttpResponse.json(command, { status: 201 })
  }),

  http.get(api('/monitors/actions'), ({ request }) => byMonitor(actions, request)),
  http.get(api('/monitors/commands'), ({ request }) => byMonitor(commands, request)),
  http.get(api('/monitors/work-actions'), ({ request }) => byMonitor(workActions, request)),
  http.get(api('/monitors/implementation-jobs'), ({ request }) => byMonitor(implementationJobs, request)),
  http.get(api('/monitors/mutations'), ({ request }) => byMonitor(mutations, request)),
]
