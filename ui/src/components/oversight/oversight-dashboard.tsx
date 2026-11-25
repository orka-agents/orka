import { useState } from 'react'
import { Eye, Pause, Play, ShieldAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { PageHeader } from '@/components/layout/page-header'
import {
  isTerminalOversightError,
  useFeedbackOverrides,
  useOversightConfig,
  useOversightDecisions,
  withOverride,
} from '@/hooks/use-oversight'
import { useOversightStream, type OversightStreamStatus } from '@/hooks/use-oversight-stream'
import { useCharterAlertHandler } from '@/hooks/use-charter-alerts'
import { ApiError, isNotImplementedError } from '@/lib/api-client'
import { cn } from '@/lib/utils'
import { groupIncidents, type Incident } from '@/lib/oversight'
import type { DecisionView } from '@/schemas/oversight'
import { AlertsButton } from './alerts-button'
import { CharterPanel } from './charter-panel'
import { CharterDisclosure } from './charter-disclosure'
import { NeedsYouList } from './needs-you'
import { PostureLine } from './posture-line'
import { DecisionInspector } from './decision-inspector'
import { DecisionTable } from './decision-table'
import { HistoryPanel } from './history-panel'
import { PostureBanner } from './posture-banner'
import { ProducersPanel } from './producers-panel'
import { QuestionMatrix } from './question-matrix'
import { SearchBox } from './search-box'
import { SelectionBar } from './selection-bar'

/** needs: what needs a person (default); activity/matrix: everything; monitors: operator material. */
export type OversightView = 'needs' | 'activity' | 'matrix' | 'monitors'

/** The tab a search shows: an explicit view, else results for a search, else Needs you. */
const effectiveView = (search: OversightSearch): OversightView => search.view ?? (search.q ? 'activity' : 'needs')

/** The decisions Needs you draws from: the server's signals that can need a person. */
const NEEDS_QUERY = 'signal:against_charter|monitor_unavailable|risky'

export interface OversightSearch {
  q?: string
  /** Absent searches all time. */
  window?: string
  /** The decision open in the inspector. */
  decision?: string
  view?: OversightView
}

interface OversightDashboardProps {
  search: OversightSearch
  onSearchChange: (next: OversightSearch) => void
}

/** Rows shown at most; older stream rows drop off. */
const STREAM_CAP = 500

const STREAM_STATUS_TEXT: Record<OversightStreamStatus, string> = {
  idle: 'Not following',
  connecting: 'Connecting…',
  live: 'Live',
  reconnecting: 'Reconnecting…',
  unavailable: 'Live stream unavailable',
  forbidden: 'Live stream not authorized',
  rejected: 'Live stream refused the search',
}

function mergeRows(streamed: DecisionView[], listed: DecisionView[]): DecisionView[] {
  const seen = new Set<string>()
  const rows: DecisionView[] = []
  for (const decision of [...streamed, ...listed]) {
    if (seen.has(decision.id)) continue
    seen.add(decision.id)
    rows.push(decision)
  }
  // Newest first by store cursor; rows without one keep their order.
  return rows.sort((a, b) => (b.seq ?? 0) - (a.seq ?? 0))
}

export function OversightDashboard({ search, onSearchChange }: OversightDashboardProps) {
  const q = search.q ?? ''
  const view = effectiveView(search)
  const { data: config } = useOversightConfig()

  const postureWindow = search.window && search.window !== 'all' ? search.window : '24h'
  // A pivot is a search across all time, shown with its results.
  const pivot = (query: string) => onSearchChange({ q: query, window: undefined, decision: undefined, view: search.view === 'matrix' ? 'matrix' : 'activity' })

  return (
    <div className="space-y-4">
      <PageHeader
        title="Oversight"
        description="Read-only: what agents did, what the monitors made of it, and how much of it was seen."
        action={<AlertsButton onOpenDecision={(id) => onSearchChange({ ...search, decision: id })} />}
      />
      <Results
        key={`${q}\u0000${search.window ?? ''}`}
        q={q}
        window={search.window}
        postureWindow={postureWindow}
        view={view}
        queryKeys={config?.queryKeys}
        onSearch={(next) => onSearchChange({ ...search, q: next || undefined, decision: undefined, view: next ? (search.view === 'matrix' ? 'matrix' : 'activity') : search.view })}
        onWindowChange={(window) => onSearchChange({ ...search, window, decision: undefined })}
        onViewChange={(next) => onSearchChange({ ...search, view: next === 'needs' && !q ? undefined : next })}
        decision={search.decision}
        onOpen={(id) => onSearchChange({ ...search, decision: id })}
        onClose={() => onSearchChange({ ...search, decision: undefined })}
        onPivot={pivot}
      />
    </div>
  )
}

interface ResultsProps {
  q: string
  window?: string
  postureWindow: string
  view: OversightView
  queryKeys?: string[]
  onSearch: (q: string) => void
  onWindowChange: (window: string | undefined) => void
  onViewChange: (view: OversightView) => void
  decision?: string
  onOpen: (id: string) => void
  onClose: () => void
  onPivot: (q: string) => void
}

/** The search box, history, and the live table or matrix for one query. */
function Results({ q, window, postureWindow, view, queryKeys, onSearch, onWindowChange, onViewChange, decision, onOpen, onClose, onPivot }: ResultsProps) {
  const { data: config } = useOversightConfig()
  const list = useOversightDecisions({ q, window })
  const overrides = useFeedbackOverrides()
  const [following, setFollowing] = useState(true)
  const [pausedAtSeq, setPausedAtSeq] = useState<number | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())

  const listError = list.error
  const terminal = isTerminalOversightError(listError)
  const grammarError = listError instanceof ApiError && listError.status === 400 ? listError.message : undefined
  const pages = terminal ? [] : (list.data?.pages ?? [])
  const listed = pages.flatMap((page) => page.items ?? [])
  const terms = pages[0]?.query
  const seedSeq = pages[0]?.items?.reduce((highest, d) => Math.max(highest, d.seq ?? 0), 0) ?? 0

  const initialIDs = new Set(listed.map((d) => d.id))
  const onStreamed = useCharterAlertHandler(onOpen, initialIDs)
  const stream = useOversightStream({ q, enabled: following && list.isSuccess && !terminal, after: seedSeq, cap: STREAM_CAP, onDecision: onStreamed })
  const streamShown = pausedAtSeq === null ? stream.decisions : stream.decisions.filter((d) => (d.seq ?? 0) <= pausedAtSeq)
  const heldBack = stream.decisions.length - streamShown.length

  const rows = mergeRows(streamShown, listed).map((d) => withOverride(d, overrides))

  // Needs you reads its own window of attention-worthy decisions, so a busy
  // feed cannot push an incident off the first page; streamed rows join it.
  const needsList = useOversightDecisions({ q: [q, NEEDS_QUERY].filter(Boolean).join(' '), window: postureWindow, limit: 200, enabled: !terminal && !grammarError })
  const needsRows = mergeRows(stream.decisions, [...(needsList.data?.pages ?? []).flatMap((page) => page.items ?? []), ...listed]).map((d) => withOverride(d, overrides))
  const incidents = groupIncidents(needsRows, config)
  const [incident, setIncident] = useState<Incident | null>(null)
  const openIncident = (next: Incident) => {
    setIncident(next)
    onOpen(next.decisions[0].id)
  }

  const selectedRows = rows.filter((d) => selected.has(d.id))
  const toggle = (id: string) => setSelected((prev) => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    return next
  })

  const listState = list.isLoading ? (
    <div className="space-y-2">{Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12 w-full" />)}</div>
  ) : terminal && !grammarError ? (
    isNotImplementedError(listError) ? (
      <EmptyState icon={ShieldAlert} headline="Oversight is not enabled" hint="This controller does not serve oversight decisions." />
    ) : (
      <ListAccessError error={listError} resource="oversight decisions" />
    )
  ) : null

  const liveControls = (
    <div className="flex flex-wrap items-center gap-2 text-xs">
      <span className="inline-flex items-center gap-1.5 text-muted-foreground" role="status" aria-live="polite">
        <span
          aria-hidden="true"
          className={cn('size-2 rounded-full', stream.status === 'live' ? 'bg-live' : stream.status === 'reconnecting' ? 'bg-status-pending' : 'bg-muted-foreground/40')}
        />
        {STREAM_STATUS_TEXT[stream.status]}
        {stream.status === 'reconnecting' && stream.error && <span className="sr-only">: {stream.error}</span>}
      </span>
      <Button
        size="xs"
        variant="outline"
        aria-pressed={pausedAtSeq !== null}
        onClick={() => setPausedAtSeq((prev) => (prev === null ? stream.decisions.reduce((h, d) => Math.max(h, d.seq ?? 0), seedSeq) : null))}
        disabled={!following}
      >
        {pausedAtSeq === null ? <><Pause aria-hidden="true" /> Pause updates</> : <><Play aria-hidden="true" /> Resume{heldBack > 0 ? ` (${heldBack} new)` : ''}</>}
      </Button>
      <Button size="xs" variant="ghost" aria-pressed={!following} onClick={() => { setFollowing((f) => !f); setPausedAtSeq(null) }}>
        {following ? 'Stop following' : 'Follow live'}
      </Button>
    </div>
  )

  return (
    <>
      <PostureLine window={postureWindow} incidents={incidents} />

      <Tabs value={view === 'matrix' ? 'activity' : view} onValueChange={(next) => onViewChange(next as OversightView)}>
        <TabsList>
          <TabsTrigger value="needs">
            Needs you
            {incidents.length > 0 && (
              <span className="rounded-full bg-status-failed-bg px-1.5 text-xs tabular-nums text-status-failed">
                <span aria-hidden="true">{incidents.length}</span>
                <span className="sr-only">{incidents.length} incidents</span>
              </span>
            )}
          </TabsTrigger>
          <TabsTrigger value="activity">All activity</TabsTrigger>
          <TabsTrigger value="monitors">Monitors</TabsTrigger>
        </TabsList>
      </Tabs>

      {view === 'needs' && (
        <div className="space-y-3">
          <CharterDisclosure window={postureWindow} onPivot={onPivot} />
          {q && (
            <p className="text-xs text-muted-foreground" role="status">
              Filtered by the search <span className="font-mono">{q}</span>.{' '}
              <button type="button" className="underline underline-offset-2" onClick={() => onSearch('')}>Clear</button>
            </p>
          )}
          <Card>
            <CardContent className="space-y-3">
              <div className="flex justify-end">{liveControls}</div>
              {listState ?? (needsList.isLoading ? <Skeleton className="h-24 w-full" /> : <NeedsYouList incidents={incidents} onOpen={openIncident} />)}
            </CardContent>
          </Card>
        </div>
      )}

      {(view === 'activity' || view === 'matrix') && (
        <div className="space-y-4">
          <SearchBox
            q={q}
            window={window}
            terms={terms}
            error={grammarError}
            queryKeys={queryKeys}
            onSubmit={onSearch}
            onWindowChange={onWindowChange}
          />

          {q && !grammarError && <HistoryPanel q={q} window={window} onPivot={onPivot} />}

          <Card>
            <CardContent className="space-y-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-1" role="group" aria-label="Layout">
                  <Button size="xs" variant={view === 'matrix' ? 'ghost' : 'secondary'} aria-pressed={view !== 'matrix'} onClick={() => onViewChange('activity')}>
                    Table
                  </Button>
                  <Button size="xs" variant={view === 'matrix' ? 'secondary' : 'ghost'} aria-pressed={view === 'matrix'} onClick={() => onViewChange('matrix')}>
                    Question × action
                  </Button>
                </div>
                {liveControls}
              </div>

              {listState ?? (grammarError ? (
                <p className="py-6 text-center text-sm text-muted-foreground">Fix the search to see results.</p>
              ) : listError && rows.length === 0 ? (
                <ListAccessError error={listError} resource="oversight decisions" />
              ) : rows.length === 0 ? (
                <EmptyState
                  icon={Eye}
                  headline={q ? 'No actions match this search' : 'No actions recorded yet'}
                  hint={q ? `Nothing matched ${window ? `in the last ${window}` : 'across all time'}.` : 'Decisions appear here as an oversight pipeline records them.'}
                />
              ) : view === 'matrix' ? (
                <QuestionMatrix decisions={rows} onOpen={onOpen} />
              ) : (
                <DecisionTable
                  decisions={rows}
                  onOpen={onOpen}
                  selectedIds={selected}
                  onToggleSelect={toggle}
                  onToggleAll={(all) => setSelected(all ? new Set(rows.map((d) => d.id)) : new Set())}
                />
              ))}

              {list.hasNextPage && !terminal && (
                <div className="flex justify-center">
                  <Button variant="outline" size="sm" onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
                    {list.isFetchingNextPage ? 'Loading…' : 'Load older'}
                  </Button>
                </div>
              )}
              {rows.length >= STREAM_CAP && (
                <p className="text-center text-xs text-muted-foreground">The live list keeps the newest {STREAM_CAP} actions; search to reach older ones.</p>
              )}
            </CardContent>
          </Card>

          <SelectionBar selected={selectedRows} resultSet={rows} query={q} onClear={() => setSelected(new Set())} />
        </div>
      )}

      {view === 'monitors' && (
        <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_22rem]">
          <div className="min-w-0 space-y-4">
            <PostureBanner window={postureWindow} />
            <CharterPanel window={postureWindow} onPivot={onPivot} />
          </div>
          <aside className="space-y-4">
            <ProducersPanel />
          </aside>
        </div>
      )}

      {/* The inspector shows a loaded row at once while it fetches the full record. */}
      <DecisionInspector
        decisionId={decision}
        initial={[...rows, ...needsRows].find((row) => row.id === decision)}
        related={incident?.decisions}
        onClose={onClose}
        onOpenDecision={onOpen}
        onPivot={onPivot}
      />
    </>
  )
}
