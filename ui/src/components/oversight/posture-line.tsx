import { useState } from 'react'
import { ChevronDown } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'
import { ShieldAlert } from 'lucide-react'
import { useOversightPosture } from '@/hooks/use-oversight'
import { isNotImplementedError } from '@/lib/api-client'
import { confidenceReasons, excludedSummary, formatSeconds, type Incident, latencyText, onlyReplayed, reasonText } from '@/lib/oversight'
import { cn } from '@/lib/utils'
import type { PostureResponse } from '@/schemas/oversight'

const CONFIDENCE_TEXT: Record<string, string> = {
  warranted: 'Warranted',
  degraded: 'Degraded',
  blind: 'Blind',
  attention_required: 'Attention required',
}

const CONFIDENCE_TONE: Record<string, string> = {
  warranted: 'text-status-succeeded',
  degraded: 'text-status-pending',
  blind: 'text-status-failed',
  attention_required: 'text-status-failed',
}

/** The state word with its main reasons inline, e.g. "Degraded: no canaries reported". */
function confidenceLabel(posture: PostureResponse): string {
  const word = CONFIDENCE_TEXT[posture.confidence.state] ?? posture.confidence.state
  const reasons = posture.confidence.state === 'warranted' ? [] : confidenceReasons(posture)
  return reasons.length ? `${word}: ${reasons.join(' · ')}` : word
}

type Item = 'confidence' | 'incidents' | 'seen' | 'blind' | 'control'

/**
 * The 10-second answer as one line; every part opens its evidence. Coverage
 * is a share of actions seen, never "warranted".
 */
export interface CharterSummary {
  /** "Demo charter charter-r4 · 8 rules · 2 broken". */
  label: string
  onOpen: () => void
}

export function PostureLineView({ posture, incidents, charter }: { posture: PostureResponse; incidents: Incident[]; charter?: CharterSummary }) {
  const [open, setOpen] = useState<Item | null>(null)
  const { basis } = posture
  const latency = basis.decisionLatency
  const coverage = basis.coverage
  const seen = basis.actions > 0 ? Math.round((coverage.sensorAttributed / basis.actions) * 100) : 0
  const blindSpots = Object.entries(coverage.blindSpots ?? {}).sort(([a], [b]) => a.localeCompare(b))
  const enforced = posture.control.state === 'enforced'
  const toggle = (item: Item) => setOpen((prev) => (prev === item ? null : item))
  const byKind = {
    charter: incidents.filter((i) => i.badge === 'against_charter').length,
    pause: incidents.filter((i) => i.badge === 'would_pause').length,
    hold: incidents.filter((i) => i.badge === 'would_hold').length,
  }

  const part = (item: Item, label: string, className?: string) => (
    <button
      type="button"
      aria-expanded={open === item}
      aria-controls={`posture-${item}`}
      onClick={() => toggle(item)}
      className={cn('inline-flex items-center gap-0.5 rounded px-1 underline-offset-2 decoration-dotted hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none', className)}
    >
      {label}
      <ChevronDown className={cn('size-3 opacity-50 transition-transform', open === item && 'rotate-180')} aria-hidden="true" />
    </button>
  )
  const dot = <span aria-hidden="true" className="whitespace-pre text-muted-foreground">{' · '}</span>

  return (
    <div role="region" aria-label="Oversight posture" className="space-y-1.5">
      <p className="flex flex-wrap items-center gap-y-1 text-sm" data-testid="posture-line">
        {part('confidence', confidenceLabel(posture), cn('font-semibold', CONFIDENCE_TONE[posture.confidence.state]))}
        {dot}
        {part('incidents', `${incidents.length} ${incidents.length === 1 ? 'incident' : 'incidents'}`)}
        {dot}
        {part('seen', `${seen}% seen`, seen < 100 ? 'text-status-pending' : undefined)}
        {dot}
        {part('blind', `${blindSpots.length} ${blindSpots.length === 1 ? 'blind spot' : 'blind spots'}`, blindSpots.length > 0 ? 'font-medium text-status-failed' : 'text-muted-foreground')}
        {dot}
        {part('control', enforced ? 'enforced' : 'audit mode', 'text-muted-foreground')}
        {onlyReplayed(latency) && (
          <>
            {dot}
            <span className="px-1 text-muted-foreground" title={latencyText(latency)} data-testid="posture-latency">
              decision latency: n/a (replayed)
            </span>
          </>
        )}
        {latency && latency.count > 0 && (
          <>
            {dot}
            <span className="px-1 text-muted-foreground" title={latencyText(latency)} data-testid="posture-latency">
              decided within ~{formatSeconds(latency.p95Seconds)}
            </span>
          </>
        )}
        {charter && (
          <>
            {dot}
            <button
              type="button"
              onClick={charter.onOpen}
              className="rounded px-1 underline-offset-2 decoration-dotted hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
            >
              {charter.label}
            </button>
          </>
        )}
      </p>

      {open === 'confidence' && (
        <div id="posture-confidence" className="rounded-md border p-3 text-sm">
          <p>{posture.confidence.summary}</p>
          {(posture.confidence.reasons?.length ?? 0) > 0 && (
            <ul className="mt-1 list-inside list-disc text-xs text-muted-foreground">
              {posture.confidence.reasons!.map((r) => <li key={r}>{reasonText(r)}</li>)}
            </ul>
          )}
        </div>
      )}
      {open === 'incidents' && (
        <div id="posture-incidents" className="rounded-md border p-3 text-sm">
          <p>{byKind.charter} charter breaks · {byKind.pause} would pause · {byKind.hold} would hold</p>
          <p className="text-xs text-muted-foreground">Grouped by agent and kind from the loaded actions. Risky-only actions are under All activity.</p>
        </div>
      )}
      {open === 'seen' && (
        <div id="posture-seen" className="rounded-md border p-3 text-sm tabular-nums">
          <p>{coverage.sensorAttributed} of {basis.actions} actions attributed in the last {posture.window || '24h'}</p>
          <p className="text-muted-foreground">{coverage.streamOK} of {basis.actions} action streams complete · {coverage.unattributedEvents} unattributed events</p>
          <p className="text-xs text-muted-foreground">{posture.visibility.summary}</p>
        </div>
      )}
      {open === 'blind' && (
        <div id="posture-blind" className="rounded-md border p-3 text-sm">
          {blindSpots.length === 0 ? (
            <p className="text-muted-foreground">No blind spot was declared.</p>
          ) : (
            <ul className="flex flex-wrap gap-1.5" aria-label="Known blind spots">
              {blindSpots.map(([code, count]) => (
                <li key={code} className="rounded-md border border-status-failed/40 px-2 py-0.5 font-mono text-xs">
                  {code} <span className="tabular-nums text-muted-foreground">×{count}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {open === 'control' && (
        <div id="posture-control" className="rounded-md border p-3 text-sm">
          <p>{enforced ? 'Enforced: decisions can hold actions.' : 'Audit mode: would pause, never holds.'}</p>
          <p className="text-muted-foreground tabular-nums">{basis.wouldHavePaused} actions would have paused</p>
        </div>
      )}

      <p className="text-xs text-muted-foreground" data-testid="posture-honesty">
        {enforced ? 'Decisions can hold actions.' : 'Audit mode: findings would pause an action; nothing is held.'}{' '}
        Effects marked kernel-observed were seen by a sensor; the rest are inferred from the agent&apos;s request.
        {posture.incomplete && ' Counts are a lower bound: the window held more decisions than were read.'}
      </p>
      {excludedSummary(coverage.excluded) && (
        <p className="text-xs text-muted-foreground" data-testid="posture-excluded" title="Seen by the monitor and left out of every action; counted once per Task">
          {excludedSummary(coverage.excluded)}
        </p>
      )}
    </div>
  )
}

export function PostureLine({ window = '24h', incidents, charter }: { window?: string; incidents: Incident[]; charter?: CharterSummary }) {
  const { data, error, isLoading } = useOversightPosture(window)
  if (isLoading) return <Skeleton className="h-6 w-96 max-w-full" />
  if (error) {
    return (
      <Card>
        <CardContent>
          {isNotImplementedError(error) ? (
            <EmptyState icon={ShieldAlert} headline="Oversight is not enabled" hint="This controller does not serve oversight decisions." />
          ) : (
            <ListAccessError error={error} resource="oversight posture" />
          )}
        </CardContent>
      </Card>
    )
  }
  if (!data) return null
  return <PostureLineView posture={data} incidents={incidents} charter={charter} />
}
