import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Eye, ShieldAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'
import { Skeleton } from '@/components/ui/skeleton'
import { useFeedbackOverrides, useOversightDecisions, withOverride } from '@/hooks/use-oversight'
import { isNotImplementedError } from '@/lib/api-client'
import { queryTerm } from '@/lib/oversight'
import { DecisionInspector } from './decision-inspector'
import { DecisionTable } from './decision-table'
import { Chip } from './signal'

/** One Task's oversight decisions, for the Task detail page. */
export function TaskOversightTab({ taskName }: { taskName: string }) {
  const navigate = useNavigate()
  const q = queryTerm('task', taskName)
  const list = useOversightDecisions({ q, limit: 100 })
  const overrides = useFeedbackOverrides()
  const [open, setOpen] = useState<string | undefined>()
  const decisions = (list.data?.pages ?? []).flatMap((page) => page.items ?? []).map((d) => withOverride(d, overrides))
  const toOversight = (query: string) => navigate({ to: '/oversight', search: { q: query } })

  const taint = new Set<string>()
  const inherited = new Set<string>()
  const blindSpots = new Set<string>()
  const charters = new Set<string>()
  let streamOK = 0
  let attributed = 0
  let unattributed = 0
  const signals: Record<string, number> = {}
  for (const d of decisions) {
    d.action.taintFlags?.forEach((flag) => taint.add(flag))
    d.action.inheritedTaint?.forEach((flag) => inherited.add(flag))
    d.coverage.blindSpots?.forEach((spot) => blindSpots.add(spot))
    if (d.charterRevision) charters.add(d.charterRevision)
    if (d.coverage.streamState === 'ok') streamOK++
    if (d.coverage.sensorState === 'events') attributed++
    unattributed += d.coverage.unattributedEvents ?? 0
    signals[d.signal] = (signals[d.signal] ?? 0) + 1
  }

  return (
    <Card>
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle className="flex items-center gap-2"><Eye className="size-4" aria-hidden="true" /> Oversight</CardTitle>
        <Button variant="outline" size="sm" onClick={() => toOversight(q)}>Open in Oversight</Button>
      </CardHeader>
      <CardContent className="space-y-4">
        {list.isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : list.error ? (
          isNotImplementedError(list.error) ? (
            <EmptyState icon={ShieldAlert} headline="Oversight is not enabled" />
          ) : (
            <ListAccessError error={list.error} resource="oversight decisions" />
          )
        ) : decisions.length === 0 ? (
          <EmptyState icon={Eye} headline="No oversight decisions for this Task" hint="Actions appear here once an oversight pipeline records them." />
        ) : (
          <>
            <div className="grid gap-3 text-sm sm:grid-cols-3">
              <div className="space-y-1" aria-label="Task coverage" role="group">
                <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Coverage</p>
                <p className="tabular-nums">{attributed} of {decisions.length} actions attributed</p>
                <p className="tabular-nums text-muted-foreground">{streamOK} of {decisions.length} streams complete · {unattributed} unattributed events</p>
                {blindSpots.size > 0 && (
                  <div className="flex flex-wrap gap-1">
                    <span className="text-xs font-medium text-status-failed">Blind spots</span>
                    {[...blindSpots].map((spot) => <Chip key={spot} className="border-status-failed/50 font-mono">{spot}</Chip>)}
                  </div>
                )}
              </div>
              <div className="space-y-1" aria-label="Task taint" role="group">
                <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Taint</p>
                {taint.size === 0 && inherited.size === 0 ? (
                  <p className="text-muted-foreground">No taint flags</p>
                ) : (
                  <div className="flex flex-wrap gap-1">
                    {[...inherited].map((flag) => <Chip key={`i-${flag}`} className="font-mono">{flag} <span className="text-muted-foreground">(inherited)</span></Chip>)}
                    {[...taint].map((flag) => <Chip key={`o-${flag}`} className="font-mono">{flag}</Chip>)}
                  </div>
                )}
              </div>
              <div className="space-y-1" aria-label="Task charter" role="group">
                <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Charter</p>
                {charters.size === 0 ? (
                  <p className="text-muted-foreground">No charter revision recorded</p>
                ) : (
                  [...charters].map((rev) => <p key={rev}>Charter <span className="font-mono text-xs">{rev}</span> (watched, not enforced)</p>)
                )}
                <p className="text-xs text-muted-foreground tabular-nums">
                  {signals.against_charter ?? 0} against the charter · {signals.risky ?? 0} risky · {signals.monitor_unavailable ?? 0} scorer unavailable
                </p>
              </div>
            </div>
            <DecisionTable decisions={decisions} onOpen={setOpen} hideTask label={`Oversight decisions for ${taskName}`} />
            {list.hasNextPage && (
              <div className="flex justify-center">
                <Button variant="outline" size="sm" onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
                  {list.isFetchingNextPage ? 'Loading…' : 'Load older'}
                </Button>
              </div>
            )}
          </>
        )}
        <DecisionInspector
          decisionId={open}
          initial={decisions.find((d) => d.id === open)}
          onClose={() => setOpen(undefined)}
          onOpenDecision={setOpen}
          onPivot={toOversight}
        />
      </CardContent>
    </Card>
  )
}
