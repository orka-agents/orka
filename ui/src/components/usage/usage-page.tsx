import { useState, type FormEvent, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { recordedTokens, usageNumber, type UsageOther, type UsageOtherSummary, type UsagePage as UsagePagination, type UsageReport, type UsageSelection, type UsageSummary, type UsageTask, type UsageTotals, type UsageWork, type UsageWorkSummary } from '@/lib/usage'
import { useUIStore } from '@/stores/ui'
import { PageHeader } from '@/components/layout/page-header'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

function initialFilters() {
  const now = new Date()
  return {
    from: new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1)).toISOString().slice(0, 10),
    until: new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1)).toISOString().slice(0, 10),
    teams: '', repository: '', model: '', kind: '',
  }
}

export function UsagePage() {
  const namespace = useUIStore((s) => s.namespace)
  return <UsagePageContent key={namespace} namespace={namespace} />
}

function UsagePageContent({ namespace }: { namespace: string }) {
  const [filters, setFilters] = useState(initialFilters)
  const [draft, setDraft] = useState(filters)
  const [page, setPage] = useState({ offset: 0, asOf: '' })
  const { data, error, isPending, isFetching } = useQuery({
    queryKey: ['usage', namespace, filters, page],
    queryFn: () => api.get<UsageReport>('/usage', { namespace, ...filters, offset: String(page.offset), asOf: page.asOf, limit: '25' }),
    refetchInterval: page.asOf ? false : 60000,
    // Keep expanded details mounted while pinning the report they already display.
    placeholderData: (previous) => page.asOf && previous?.selection.asOf === page.asOf && previous.page.offset === page.offset ? previous : undefined,
  })
  function apply(event: FormEvent) {
    event.preventDefault()
    setFilters({ ...draft })
    setPage({ offset: 0, asOf: '' })
  }
  function pinReport() {
    if (data) setPage((current) => ({ ...current, asOf: data.selection.asOf }))
  }
  return (
    <div className="space-y-6">
      <PageHeader title="Usage and PR outcomes" description="Follow recorded model usage from the original issue through review and merge." />
      <form onSubmit={apply} className="grid items-end gap-3 rounded-lg border bg-card p-4 sm:grid-cols-2 xl:grid-cols-4">
        <label className="space-y-1 text-sm">Requests started from
          <Input aria-label="Requests started from" type="date" required value={draft.from} onChange={(e) => setDraft({ ...draft, from: e.target.value })} />
        </label>
        <label className="space-y-1 text-sm">Started before, exclusive
          <Input aria-label="Started before, exclusive" type="date" required value={draft.until} onChange={(e) => setDraft({ ...draft, until: e.target.value })} />
        </label>
        <label className="space-y-1 text-sm">Team namespace
          <Input aria-label="Team namespace" aria-describedby="usage-team-scope" placeholder={namespace || 'Current namespace'} value={draft.teams} onChange={(e) => setDraft({ ...draft, teams: e.target.value })} />
          <span id="usage-team-scope" className="block text-xs text-muted-foreground">Use this server's team namespace.</span>
        </label>
        <label className="space-y-1 text-sm">Repository
          <Input aria-label="Repository" placeholder="owner/repository" value={draft.repository} onChange={(e) => setDraft({ ...draft, repository: e.target.value })} />
        </label>
        <label className="space-y-1 text-sm">Model used by the request
          <Input aria-label="Model used by the request" placeholder="All models" value={draft.model} onChange={(e) => setDraft({ ...draft, model: e.target.value })} />
        </label>
        <label className="space-y-1 text-sm">Work type
          <select aria-label="Work type" value={draft.kind} onChange={(e) => setDraft({ ...draft, kind: e.target.value })} className="flex h-9 w-full rounded-md border border-input bg-background px-3">
            <option value="">All work</option><option value="issue">Issue delivery</option><option value="pull_request">Existing PR review and repair</option>
          </select>
        </label>
        <Button type="submit" disabled={isFetching} className="sm:col-start-2 xl:col-start-4">Apply filters</Button>
      </form>
      {isPending && <p role="status">Loading usage report...</p>}
      {error && <p role="alert" className="text-destructive">Usage report unavailable. {error.message}</p>}
      {data && !error && <>
        <p className="text-sm text-muted-foreground">
          Requests started {new Date(data.selection.from).toLocaleDateString(undefined, { timeZone: 'UTC' })} through {new Date(data.selection.until).toLocaleDateString(undefined, { timeZone: 'UTC' })}, exclusive, UTC.
          {' '}Usage and outcomes recorded through {new Date(data.selection.asOf).toLocaleString()}.
          {' '}All follow-up usage for these requests is included, even after the selected start period.
        </p>
        <OutcomeSummary summary={data.summary} />
        <Card>
          <CardHeader><CardTitle>Team summary</CardTitle></CardHeader>
          <CardContent><Table>
            <TableHeader><TableRow>
              <TableHead>Team</TableHead><TableHead>Requests</TableHead><TableHead>Recorded tokens</TableHead>
              <TableHead>PRs opened</TableHead><TableHead>Ready now</TableHead><TableHead>Merged</TableHead><TableHead>Tokens / merged PR</TableHead>
            </TableRow></TableHeader>
            <TableBody>{data.teams.map((team) => <TableRow key={team.namespace}>
              <TableCell>{team.namespace}</TableCell><TableCell>{team.summary.workRequests}</TableCell>
              <TableCell className="font-mono tabular-nums">{recordedTokens(team.summary)}</TableCell>
              <TableCell>{team.summary.prsOpened}</TableCell><TableCell>{team.summary.prsReady}</TableCell><TableCell>{team.summary.prsMerged}</TableCell>
              <TableCell className="font-mono tabular-nums">{usageNumber(team.summary.tokensPerPRMerged)}</TableCell>
            </TableRow>)}</TableBody>
          </Table></CardContent>
        </Card>
        <section className="space-y-3" aria-label="Work requests">
          <h2 className="text-lg font-semibold">Work requests</h2>
          <p className="text-sm text-muted-foreground">Expand a request to inspect attempts and measurement gaps. Shared review usage appears in each linked request and counts once in team totals.</p>
          {data.works.length === 0 && <p>No issue-delivery requests in this cohort. Change the filters or run an issue-to-PR workflow to begin recording.</p>}
          {data.works.map((work) => <WorkDetails key={work.id} work={work} selection={data.selection} onPaginate={pinReport} />)}
          <PageControls label="Work requests" page={data.page} disabled={isFetching} onChange={(offset) => setPage({ offset, asOf: data.selection.asOf })} />
        </section>
        <section className="space-y-3" aria-label="Other team usage">
          <h2 className="text-lg font-semibold">Other team usage</h2>
          <p className="text-sm text-muted-foreground">Activity started in the selected period that is excluded from this delivery cohort.</p>
          {data.otherWork.map((group) => <OtherDetails key={`${group.category}/${JSON.stringify(filters)}`} group={group} namespace={namespace} selection={data.selection} onPaginate={pinReport} />)}
        </section>
        {data.retainedSince && <p className="text-xs text-muted-foreground">Inactive cohorts may expire before {new Date(data.retainedSince).toLocaleDateString()}. Retained requests keep their earlier attempts.</p>}
      </>}
    </div>
  )
}

function otherTitle(category: string) {
  if (category === 'review_only') return 'Existing PR review and repair'
  if (category === 'other_requests') return 'Other requests'
  return 'Chat and usage without a work reference'
}

function OutcomeSummary({ summary }: { summary: UsageSummary }) {
  return <Card>
    <CardHeader><CardTitle>Tokens per merged PR</CardTitle></CardHeader>
    <CardContent className="space-y-5">
      <div className="flex flex-col gap-5 lg:flex-row lg:items-end lg:justify-between">
        <div className="space-y-2">
          <p className="font-mono text-3xl font-semibold tabular-nums tracking-tight">
            {summary.prsMerged === 0 ? 'No PRs merged yet' : summary.tokensPerPRMerged == null ? 'Usage unavailable' : usageNumber(summary.tokensPerPRMerged)}
          </p>
          <p className="text-sm text-muted-foreground">{recordedTokens(summary)}{summary.completeness !== 'unavailable' && ' recorded tokens'} across {summary.workRequests} requests, including unsuccessful work.</p>
          {summary.tokensPerPRMerged != null && <p className="font-mono text-xs text-muted-foreground">{usageNumber(summary.totalTokens)} tokens / {summary.prsMerged} merged PRs</p>}
        </div>
        <dl className="grid grid-cols-3 gap-6 text-sm">
          <div><dt className="text-muted-foreground">Opened</dt><dd className="font-mono text-2xl">{summary.prsOpened}</dd></div>
          <div><dt className="text-muted-foreground">Ready now</dt><dd className="font-mono text-2xl">{summary.prsReady}</dd></div>
          <div><dt className="text-muted-foreground">Merged</dt><dd className="font-mono text-2xl">{summary.prsMerged}</dd></div>
        </dl>
      </div>
      <p className="text-sm text-muted-foreground">{summary.unfinishedWork} requests unfinished. {summary.prsClosed} PRs closed without merge. {summary.prsAssisted} developer-created PRs with linked assistance.
        {' '}Opened and merged are historical counts; ready is current. These counts overlap.</p>
      <div className="grid gap-3 border-t pt-4 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <p>Tokens / opened PR <span className="block font-mono">{usageNumber(summary.tokensPerPROpened)}</span></p>
        <p>Input / output <span className="block font-mono">{summary.completeness === 'unavailable' ? 'Unavailable' : `${usageNumber(summary.inputTokens)} / ${usageNumber(summary.outputTokens)}`}</span></p>
        <p>Cached reads / writes <span className="block font-mono">{usageNumber(summary.cachedUsageReported ? summary.cachedInputTokens : null)} / {usageNumber(summary.cacheWriteUsageReported ? summary.cacheWriteInputTokens : null)}</span></p>
        <p>Model cost <span className="block">{summary.modelCost}</span></p>
      </div>
      <MeasurementCoverage usage={summary} />
      <p className="text-xs text-muted-foreground">Cached tokens are already included in input. Model filtering selects whole requests and keeps their full usage. These figures measure observed model usage, without infrastructure cost or human review time.</p>
    </CardContent>
  </Card>
}

function MeasurementCoverage({ usage }: { usage: UsageTotals }) {
  return <p className="text-sm" role="status">
    Measurement {usage.completeness}. {usage.reportedMeasurements} of {usage.measurements} measurements contain usage.
    {' '}{usage.calls} model calls and {usage.attempts} agent attempts; attempt reports do not establish call counts.
    {' '}{usage.missingMeasurements} missing, {usage.partialMeasurements} partial. Missing usage is excluded from recorded totals and may increase the ratios.
    {usage.estimatedTokens > 0 && ` ${usageNumber(usage.estimatedTokens)} tokens are estimates.`}
  </p>
}

function selectionParams(selection: UsageSelection): Record<string, string> {
  return { from: selection.from, until: selection.until, asOf: selection.asOf,
    repository: selection.repository || '', model: selection.model || '', kind: selection.kind || '' }
}

function PageControls({ label, page, onChange, disabled = false }: { label: string; page: UsagePagination; onChange: (offset: number) => void; disabled?: boolean }) {
  if (page.total <= page.limit) return null
  return <nav aria-label={`${label} pages`} className="flex flex-wrap items-center gap-3 text-sm">
    <Button variant="outline" size="sm" aria-label={`Previous ${label}`} disabled={disabled || page.offset === 0} onClick={() => onChange(Math.max(0, page.offset - page.limit))}>Previous</Button>
    <span>{Math.min(page.offset + 1, page.total)} to {Math.min(page.offset + page.limit, page.total)} of {page.total}</span>
    <Button variant="outline" size="sm" aria-label={`Next ${label}`} disabled={disabled || page.offset + page.limit >= page.total} onClick={() => onChange(page.offset + page.limit)}>Next</Button>
  </nav>
}

function PagedItems<T>({ label, items, renderItem, onPaginate }: { label: string; items: T[]; renderItem: (item: T) => ReactNode; onPaginate: () => void }) {
  const [offset, setOffset] = useState(0)
  const start = Math.min(offset, Math.max(0, Math.ceil(items.length / 25) - 1) * 25)
  return <>
    {items.slice(start, start + 25).map(renderItem)}
    <PageControls label={label} page={{ offset: start, limit: 25, total: items.length }} onChange={(nextOffset) => { onPaginate(); setOffset(nextOffset) }} />
  </>
}

function WorkDetails({ work, selection, onPaginate }: { work: UsageWorkSummary; selection: UsageSelection; onPaginate: () => void }) {
  const [open, setOpen] = useState(false)
  return <details className="rounded-lg border bg-card p-4" onToggle={(event) => setOpen(event.currentTarget.open)}>
    <summary className="cursor-pointer text-sm font-medium">
      {work.repository} #{work.number}<span className="ml-3 text-muted-foreground">{work.namespace}</span>
      <span className="ml-3 font-mono">{recordedTokens(work.summary)}</span>
    </summary>
    {open && <WorkContent work={work} selection={selection} onPaginate={onPaginate} />}
  </details>
}

function WorkContent({ work: summary, selection, onPaginate }: { work: UsageWorkSummary; selection: UsageSelection; onPaginate: () => void }) {
  const params = { namespace: summary.namespace, ...selectionParams(selection) }
  const { data, error, isPending } = useQuery({
    queryKey: ['usage-work', summary.id, params],
    queryFn: () => api.get<{ work: UsageWork }>(`/usage/work/${encodeURIComponent(summary.id)}`, params),
    staleTime: Infinity,
  })
  if (error) return <p role="alert" className="mt-3 text-destructive">Work details unavailable. {error.message}</p>
  if (isPending) return <p role="status" className="mt-3">Loading work details...</p>
  const work = data.work
  return <div className="mt-4 space-y-4">
      <div className="flex flex-wrap gap-3 text-sm">
        <a className="text-primary underline" href={`https://github.com/${work.repository}/issues/${work.number}`} target="_blank" rel="noreferrer">Open issue #{work.number}</a>
        <span>Started {new Date(work.startedAt).toLocaleString()}</span>
        <span>Models: {work.models?.join(', ') || 'Unavailable'}</span>
      </div>
      <MeasurementCoverage usage={work.summary} />
      <PagedItems label="Pull requests" items={work.pullRequests || []} onPaginate={onPaginate} renderItem={(pr) => <div key={`${pr.repository}#${pr.number}`} className="space-y-1 border-l-2 border-primary/40 pl-3 text-sm">
        <a href={pr.url} target="_blank" rel="noreferrer" className="text-primary underline">PR #{pr.number}</a>
        <span className="ml-2">{pr.origin === 'created' ? 'Created through Orka' : pr.origin === 'assisted' ? 'Linked assistance' : 'Review only'}</span>
        <Badge variant="outline" className="ml-2">{pr.ready ? 'Ready for merge' : pr.state}</Badge>
        {pr.headSHA && <p className="font-mono text-xs">Head {pr.headSHA}</p>}
        {pr.readinessReason && <p className="text-muted-foreground">{pr.readinessReason}</p>}
        {pr.observedAt && !pr.observedAt.startsWith('0001') && <p className="text-xs text-muted-foreground">Checked {new Date(pr.observedAt).toLocaleString()}</p>}
      </div>} />
      <PagedItems label="Tasks" items={work.tasks || []} onPaginate={onPaginate} renderItem={(task) => <TaskDetails key={`${task.namespace}/${task.taskUID}`} task={task} onPaginate={onPaginate} />} />
    </div>
}

function OtherDetails({ group, namespace, selection, onPaginate }: { group: UsageOtherSummary; namespace: string; selection: UsageSelection; onPaginate: () => void }) {
  const [open, setOpen] = useState(false)
  return <details className="rounded-lg border bg-card p-4" onToggle={(event) => setOpen(event.currentTarget.open)}>
    <summary className="cursor-pointer text-sm font-medium">{otherTitle(group.category)} <span className="ml-2 font-mono text-muted-foreground">{group.taskCount ? recordedTokens(group.usage) : 'No recorded activity'}</span></summary>
    {open && <div className="mt-3 space-y-3">
      <p className="text-sm text-muted-foreground">{group.explanation}</p>
      {group.taskCount > 0 && <OtherContent group={group} namespace={namespace} selection={selection} onPaginate={onPaginate} />}
    </div>}
  </details>
}

function OtherContent({ group, namespace, selection, onPaginate }: { group: UsageOtherSummary; namespace: string; selection: UsageSelection; onPaginate: () => void }) {
  const [offset, setOffset] = useState(0)
  const params = { namespace, ...selectionParams(selection), teams: selection.teams.join(','), offset: String(offset), limit: '25' }
  const { data, error, isPending, isFetching } = useQuery({
    queryKey: ['usage-other', group.category, params],
    queryFn: () => api.get<{ otherWork: UsageOther }>(`/usage/other/${encodeURIComponent(group.category)}`, params),
    staleTime: Infinity,
  })
  if (error) return <p role="alert" className="text-destructive">Usage details unavailable. {error.message}</p>
  if (isPending) return <p role="status">Loading usage details...</p>
  return <>
    {data.otherWork.tasks?.map((task) => <TaskDetails key={`${task.namespace}/${task.taskUID}`} task={task} onPaginate={onPaginate} />)}
    <PageControls label={`${otherTitle(group.category)} Tasks`} page={data.otherWork.page} disabled={isFetching} onChange={(nextOffset) => { onPaginate(); setOffset(nextOffset) }} />
  </>
}

function TaskDetails({ task, onPaginate }: { task: UsageTask; onPaginate: () => void }) {
  const [open, setOpen] = useState(false)
  const setNamespace = useUIStore((s) => s.setNamespace)
  return <details className="rounded-md border p-3 text-sm" onToggle={(event) => setOpen(event.currentTarget.open)}>
    <summary className="cursor-pointer">{task.taskName || 'Model call'} <span className="text-muted-foreground">{task.role} · {task.phase}</span>
      <span className="ml-3 font-mono">{recordedTokens(task.usage)}</span>{task.shared && <Badge variant="outline" className="ml-2">Shared work</Badge>}
    </summary>
    {open && <div className="mt-3 space-y-3">
      {task.taskName && <Link to="/tasks/$taskId" params={{ taskId: task.taskName }} onClick={() => setNamespace(task.namespace)} className="text-primary underline">Open Task {task.taskName}</Link>}
      {task.sessionName && <Link to="/sessions/$sessionId" params={{ sessionId: task.sessionName }} onClick={() => setNamespace(task.namespace)} className="ml-3 text-primary underline">Session {task.sessionName}</Link>}
      <PagedItems label="Measurements" items={task.measurements} onPaginate={onPaginate} renderItem={(m) => <div key={m.id} className="space-y-1 border-t pt-3">
        <p>{m.provider || 'Provider unavailable'} / {m.model || 'Model unavailable'} · {m.source} · {m.scope} · {m.status}</p>
        <p className="break-all font-mono text-xs">Attempt {m.attemptID || m.id}</p>
        <p>Input {usageNumber(m.inputTokens)} · Output {usageNumber(m.outputTokens)} · Cached reads {usageNumber(m.cachedInputTokens)} · Cached writes {usageNumber(m.cacheWriteInputTokens)}</p>
        <p>Measurement {m.completeness}. {m.gap}</p>
        <p className="text-xs text-muted-foreground">Recorded {new Date(m.observedAt).toLocaleString()}</p>
      </div>} />
    </div>}
  </details>
}
