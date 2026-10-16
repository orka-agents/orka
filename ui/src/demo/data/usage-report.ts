import type {
  UsageMeasurement, UsageOther, UsageOtherSummary, UsagePR, UsagePage, UsageReport, UsageSelection, UsageSummary, UsageTask,
  UsageTotals, UsageWork,
} from '@/lib/usage'
import type { UsageLedger, UsageLinkRecord, UsagePRRecord, UsageRecordTask, UsageWorkRecord } from './usage'

// A browser-side port of the controller's usage report (internal/usage/report.go):
// the same cohort rules, PR outcome counting, shared-usage handling, and
// other-usage categories, applied to the demo ledger.

export interface UsageFilter {
  teams: string[]
  repository?: string
  model?: string
  kind?: string
  from: Date
  until: Date
  asOf: Date
}

export const OTHER_CATEGORIES = ['review_only', 'other_requests', 'unassociated'] as const

const EXPLANATIONS: Record<(typeof OTHER_CATEGORIES)[number], string> = {
  review_only: 'Review or repair of existing PRs; excluded from PR delivery counts',
  other_requests: 'Work outside the selected request cohort',
  unassociated: 'Chat or usage without a verified work reference; excluded from PR delivery counts',
}

const TERMINAL_PHASES = new Set(['Succeeded', 'Failed', 'Cancelled'])

type MeasuredTask = UsageRecordTask

interface BuiltWork extends UsageWork {
  unfinished: boolean
}

const time = (iso: string) => new Date(iso).getTime()
const prKey = (repository: string, number: number) => `${repository.toLowerCase()}#${number}`
const sameRepository = (a: string | undefined, b: string) => (a ?? '').toLowerCase() === b.toLowerCase()

export function totals(measurements: UsageMeasurement[]): UsageTotals {
  const total: UsageTotals = {
    inputTokens: 0, outputTokens: 0, cachedInputTokens: 0, cacheWriteInputTokens: 0,
    cachedUsageReported: measurements.length > 0, cacheWriteUsageReported: measurements.length > 0,
    totalTokens: 0, estimatedTokens: 0, measurements: 0, reportedMeasurements: 0, missingMeasurements: 0,
    partialMeasurements: 0, calls: 0, attempts: 0, completeness: 'unavailable', modelCost: 'Price unavailable',
  }
  for (const m of measurements) {
    total.measurements++
    if (m.scope === 'call') total.calls++
    else total.attempts++
    if (m.completeness === 'complete') total.reportedMeasurements++
    else if (m.completeness === 'partial') {
      total.reportedMeasurements++
      total.partialMeasurements++
    } else total.missingMeasurements++
    total.inputTokens += m.inputTokens ?? 0
    total.outputTokens += m.outputTokens ?? 0
    total.cachedInputTokens += m.cachedInputTokens ?? 0
    total.cacheWriteInputTokens += m.cacheWriteInputTokens ?? 0
    if (m.source === 'estimate') total.estimatedTokens += (m.inputTokens ?? 0) + (m.outputTokens ?? 0)
    total.cachedUsageReported = total.cachedUsageReported && m.cachedInputTokens != null
    total.cacheWriteUsageReported = total.cacheWriteUsageReported && m.cacheWriteInputTokens != null
  }
  total.totalTokens = total.inputTokens + total.outputTokens
  if (total.reportedMeasurements > 0) {
    total.completeness = total.missingMeasurements > 0 || total.partialMeasurements > 0 ? 'partial' : 'complete'
  }
  return total
}

function summarize(works: BuiltWork[], tasks: Iterable<MeasuredTask>): UsageSummary {
  const summary: UsageSummary = {
    ...totals([...tasks].flatMap((task) => task.measurements)),
    workRequests: works.length, unfinishedWork: 0, prsOpened: 0, prsReady: 0, prsMerged: 0, prsClosed: 0, prsAssisted: 0,
    tokensPerPROpened: null, tokensPerPRMerged: null,
  }
  const prs = new Map<string, UsagePR>()
  for (const work of works) {
    for (const pr of work.pullRequests ?? []) {
      const key = prKey(pr.repository, pr.number)
      const previous = prs.get(key)
      prs.set(key, previous?.origin === 'created' ? { ...pr, origin: 'created' } : pr)
    }
    if (work.unfinished) summary.unfinishedWork++
  }
  for (const pr of prs.values()) {
    if (pr.origin === 'assisted') {
      summary.prsAssisted++
      continue
    }
    if (pr.origin !== 'created') continue
    summary.prsOpened++
    if (pr.ready) summary.prsReady++
    if (pr.mergedAt) summary.prsMerged++
    else if (pr.state === 'closed') summary.prsClosed++
  }
  if (summary.prsOpened > 0 && summary.reportedMeasurements > 0) summary.tokensPerPROpened = summary.totalTokens / summary.prsOpened
  if (summary.prsMerged > 0 && summary.reportedMeasurements > 0) summary.tokensPerPRMerged = summary.totalTokens / summary.prsMerged
  return summary
}

/** The ledger as it stood at `asOf`: later tasks, measurements, links, and merges are not yet known. */
function asOfView(ledger: UsageLedger, asOf: Date) {
  const cutoff = asOf.getTime()
  const tasks = ledger.tasks
    .filter((task) => time(task.startedAt) <= cutoff)
    .map((task) => ({ ...task, measurements: task.measurements.filter((m) => time(m.observedAt) <= cutoff) }))
  const prs = new Map<string, UsagePRRecord>()
  for (const pr of ledger.pullRequests) {
    if (time(pr.observedAt) > cutoff) continue
    const mergedLater = pr.mergedAt && time(pr.mergedAt) > cutoff
    prs.set(prKey(pr.repository, pr.number), mergedLater ? { ...pr, state: 'open', mergedAt: undefined } : pr)
  }
  const links = ledger.links.filter((link) => time(link.linkedAt) <= cutoff)
  return { tasks, prs, links }
}

const inPeriod = (iso: string, filter: UsageFilter) => {
  const t = time(iso)
  return t >= filter.from.getTime() && t < filter.until.getTime() && t <= filter.asOf.getTime()
}

const usesModel = (tasks: Iterable<MeasuredTask>, model: string) =>
  [...tasks].some((task) => task.measurements.some((m) => m.model === model))

function taskOutput(task: MeasuredTask, shared: boolean): UsageTask & Record<string, unknown> {
  return { ...task, usage: totals(task.measurements), shared }
}

function buildWork(
  request: UsageWorkRecord, members: Map<string, MeasuredTask>, links: UsageLinkRecord[], prs: Map<string, UsagePRRecord>, details: boolean,
): BuiltWork {
  let unfinished = members.size === 0
  const pullRequests: UsagePR[] = links.map((link) => {
    const pr: UsagePRRecord = prs.get(prKey(link.repository, link.number)) ?? {
      namespace: request.namespace, repository: link.repository, number: link.number,
      url: `https://github.com/${link.repository}/pull/${link.number}`, state: 'open', headSHA: '', ready: false,
      readinessReason: 'GitHub state has not been observed', observedAt: '0001-01-01T00:00:00Z',
    }
    if (pr.state === 'open') unfinished = true
    return { ...pr, origin: link.origin }
  }).sort((a, b) => a.number - b.number)
  const tasks: UsageTask[] = []
  const models = new Set<string>()
  for (const task of members.values()) {
    if (!TERMINAL_PHASES.has(task.phase)) unfinished = true
    if (!details) continue
    tasks.push(taskOutput(task, task.workID !== request.id))
    for (const m of task.measurements) if (m.model) models.add(m.model)
  }
  tasks.sort((a, b) => time(a.startedAt) - time(b.startedAt) || a.taskUID.localeCompare(b.taskUID))
  const built: BuiltWork = { ...request, summary: undefined as unknown as UsageSummary, pullRequests, unfinished }
  built.summary = summarize([built], members.values())
  if (details) {
    built.tasks = tasks
    built.models = [...models].sort()
  }
  return built
}

function otherWork(tasks: MeasuredTask[], selected: Set<string>, ledger: UsageLedger, filter: UsageFilter, detailCategory: string) {
  const workKinds = new Map(ledger.works.map((work) => [work.id, work.kind as string]))
  const groups = OTHER_CATEGORIES.map((category) => ({ category, tasks: [] as MeasuredTask[] }))
  for (const task of tasks) {
    if (task.measurements.length === 0 || selected.has(task.taskUID) || !inPeriod(task.startedAt, filter)) continue
    if (filter.repository && !sameRepository(task.repository, filter.repository)) continue
    if (filter.model && !usesModel([task], filter.model)) continue
    const workKind = task.workID ? workKinds.get(task.workID) : undefined
    const kind = workKind || (task.prNumber ? 'pull_request' : '')
    if (filter.kind && kind !== filter.kind) continue
    const index = workKind === 'pull_request' || task.prNumber ? 0 : task.workID ? 1 : 2
    groups[index].tasks.push(task)
  }
  return groups.map(({ category, tasks: members }) => {
    members.sort((a, b) => a.taskUID.localeCompare(b.taskUID))
    const group: UsageOtherSummary & { tasks?: UsageTask[] } = {
      category, explanation: EXPLANATIONS[category], usage: totals(members.flatMap((task) => task.measurements)), taskCount: members.length,
    }
    if (detailCategory === category) group.tasks = members.map((task) => taskOutput(task, false))
    return group
  })
}

function build(ledger: UsageLedger, filter: UsageFilter, details: boolean, otherCategory: string) {
  const view = asOfView(ledger, filter.asOf)
  const teams = new Set(filter.teams)
  const tasks = view.tasks.filter((task) => teams.has(task.namespace))
  const byWork = new Map<string, MeasuredTask[]>()
  const byPR = new Map<string, MeasuredTask[]>()
  for (const task of tasks) {
    if (task.workID) byWork.set(task.workID, [...(byWork.get(task.workID) ?? []), task])
    if (task.prNumber && task.repository) {
      const key = prKey(task.repository, task.prNumber)
      byPR.set(key, [...(byPR.get(key) ?? []), task])
    }
  }
  const selected = new Map<string, MeasuredTask>()
  const teamTasks = new Map<string, Map<string, MeasuredTask>>()
  const teamWorks = new Map<string, BuiltWork[]>()
  const works: BuiltWork[] = []
  for (const request of ledger.works) {
    if (!teams.has(request.namespace) || request.kind !== 'issue' || !inPeriod(request.startedAt, filter)) continue
    if (filter.repository && !sameRepository(request.repository, filter.repository)) continue
    if (filter.kind && filter.kind !== 'issue') continue
    const links = view.links.filter((link) => link.workID === request.id)
    const members = new Map<string, MeasuredTask>()
    for (const task of byWork.get(request.id) ?? []) members.set(task.taskUID, task)
    for (const link of links) for (const task of byPR.get(prKey(link.repository, link.number)) ?? []) members.set(task.taskUID, task)
    if (filter.model && !usesModel(members.values(), filter.model)) continue
    const built = buildWork(request, members, links, view.prs, details)
    for (const [key, task] of members) {
      selected.set(key, task)
      const team = teamTasks.get(request.namespace) ?? new Map<string, MeasuredTask>()
      team.set(key, task)
      teamTasks.set(request.namespace, team)
    }
    works.push(built)
    teamWorks.set(request.namespace, [...(teamWorks.get(request.namespace) ?? []), built])
  }
  works.sort((a, b) => time(a.startedAt) - time(b.startedAt) || a.id.localeCompare(b.id))
  return {
    works,
    summary: summarize(works, selected.values()),
    teams: filter.teams.map((namespace) => ({
      namespace, summary: summarize(teamWorks.get(namespace) ?? [], teamTasks.get(namespace)?.values() ?? []),
    })),
    otherWork: otherWork(tasks, new Set(selected.keys()), ledger, filter, otherCategory),
  }
}

function selection(filter: UsageFilter): UsageSelection {
  return {
    teams: filter.teams,
    ...(filter.repository && { repository: filter.repository }),
    ...(filter.kind && { kind: filter.kind }),
    ...(filter.model && { model: filter.model }),
    from: filter.from.toISOString(),
    until: filter.until.toISOString(),
    asOf: filter.asOf.toISOString(),
  }
}

function pageBounds(page: Omit<UsagePage, 'total'>, total: number) {
  const start = Math.min(page.offset, total)
  return [start, Math.min(start + page.limit, total)] as const
}

/** A compact work row: the page lists PRs and tasks only in the work detail. */
function compactWork(built: BuiltWork): UsageWork {
  const work: Partial<BuiltWork> = { ...built }
  delete work.unfinished
  delete work.pullRequests
  return work as UsageWork
}

/** GET /usage: totals over the whole selection with one page of compact work rows. */
export function usageReport(ledger: UsageLedger, filter: UsageFilter, page: Omit<UsagePage, 'total'>): UsageReport {
  const report = build(ledger, filter, false, '')
  const [start, end] = pageBounds(page, report.works.length)
  return {
    selection: selection(filter),
    retainedSince: ledger.retainedSince,
    summary: report.summary,
    teams: report.teams,
    works: report.works.slice(start, end).map(compactWork),
    otherWork: report.otherWork,
    page: { ...page, total: report.works.length },
  }
}

/** GET /usage/work/:id: one request with its tasks, PRs, and models, or undefined. */
export function usageWork(ledger: UsageLedger, filter: UsageFilter, id: string) {
  const found = build(ledger, filter, true, '').works.find((work) => work.id === id)
  if (!found) return undefined
  const work: Partial<BuiltWork> = { ...found }
  delete work.unfinished
  return { selection: selection(filter), work: work as UsageWork }
}

/** GET /usage/other/:category: one page of the category's tasks. */
export function usageOther(ledger: UsageLedger, filter: UsageFilter, category: string, page: Omit<UsagePage, 'total'>) {
  const group = build(ledger, filter, false, category).otherWork.find((item) => item.category === category)
  if (!group) return undefined
  const [start, end] = pageBounds(page, group.taskCount)
  const otherWork: UsageOther = { ...group, tasks: group.tasks?.slice(start, end), page: { ...page, total: group.taskCount } }
  return { selection: selection(filter), otherWork }
}
