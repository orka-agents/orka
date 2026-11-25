import { AlertTriangle, EyeOff, ShieldAlert } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'
import { Skeleton } from '@/components/ui/skeleton'
import { useOversightConfig, useOversightPosture } from '@/hooks/use-oversight'
import { isNotImplementedError } from '@/lib/api-client'
import { cleanlyCovered, demoCharterRevisions as demoRevisions, excludedText, incompleteEvidenceText, latencyText, reasonText } from '@/lib/oversight'
import { formatTimestamp, timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import type { PostureResponse } from '@/schemas/oversight'

const CONFIDENCE: Record<string, { label: string; className: string }> = {
  warranted: { label: 'Warranted', className: 'border-status-succeeded/40 bg-status-succeeded-bg text-status-succeeded' },
  degraded: { label: 'Degraded', className: 'border-status-pending/40 bg-status-pending-bg text-status-pending' },
  blind: { label: 'Blind', className: 'border-dashed border-status-failed text-status-failed' },
  attention_required: { label: 'Attention required', className: 'border-status-failed/40 bg-status-failed-bg text-status-failed' },
}

// Coverage is described by what was seen, never as "warranted": full
// coverage only says nothing went unseen, not that the monitors are right.
const COVERAGE: Record<string, string> = {
  full: 'Full coverage',
  partial: 'Partial coverage',
  blind: 'Blind: some actions went unseen',
}

function percent(part: number, whole: number) {
  if (whole <= 0) return '0%'
  return `${Math.round((part / whole) * 100)}%`
}

export function PostureView({ posture, demoCharterRevisions = [] }: { posture: PostureResponse; demoCharterRevisions?: string[] }) {
  const { basis } = posture
  const coverage = basis.coverage
  const confidence = CONFIDENCE[posture.confidence.state] ?? { label: posture.confidence.state, className: 'border-border' }
  const blindSpots = Object.entries(coverage.blindSpots ?? {}).sort(([a], [b]) => a.localeCompare(b))
  const enforced = posture.control.state === 'enforced'
  const watchNote = enforced ? '(in force)' : '(watched, not enforced)'
  const signals = basis.signals ?? {}

  return (
    <Card aria-label="Oversight posture" role="region">
      <CardContent className="space-y-4">
        <div className="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
          <div className="min-w-0 space-y-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Confidence</span>
              <span data-testid="confidence-state" className={cn('rounded-full border px-2.5 py-0.5 text-sm font-semibold', confidence.className)}>
                {confidence.label}
              </span>
              <span className="text-xs text-muted-foreground">last {posture.window || '24h'}</span>
            </div>
            <p className="text-sm">{posture.confidence.summary}</p>
            {(posture.confidence.reasons?.length ?? 0) > 0 && (
              <ul className="flex flex-wrap gap-1.5" aria-label="Confidence reasons">
                {posture.confidence.reasons!.map((reason) => (
                  <li key={reason} className="rounded-md bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">{reasonText(reason)}</li>
                ))}
              </ul>
            )}
          </div>
          <dl className="grid shrink-0 grid-cols-3 gap-x-6 gap-y-1 text-sm sm:grid-cols-6 lg:grid-cols-3">
            <Stat label="Actions" value={basis.actions} />
            <Stat label="Tasks" value={basis.tasks} />
            <Stat label="Agents" value={basis.agents} />
            <Stat label="Risky" value={signals.risky ?? 0} />
            <Stat label="Against charter" value={signals.against_charter ?? 0} />
            <Stat label="Unreviewed" value={basis.unreviewedFlagged} />
          </dl>
        </div>

        <div className="grid gap-3 md:grid-cols-3">
          <section aria-label="Coverage" className="space-y-1.5 rounded-md border p-3" data-testid="posture-coverage">
            <h3 className="flex items-center gap-1.5 text-sm font-semibold">
              {posture.visibility.state !== 'full' && <EyeOff className="size-4 text-status-failed" aria-hidden="true" />}
              {COVERAGE[posture.visibility.state] ?? 'Coverage unknown'}
              <span className="font-normal text-muted-foreground">· {percent(cleanlyCovered(coverage), basis.actions)} cleanly seen</span>
            </h3>
            <p className="text-sm tabular-nums">{coverage.sensorAttributed} of {basis.actions} actions attributed</p>
            {incompleteEvidenceText(coverage.evidenceIncomplete) && (
              <p className="text-sm font-medium tabular-nums text-status-pending" data-testid="coverage-evidence-incomplete">
                {incompleteEvidenceText(coverage.evidenceIncomplete)}
              </p>
            )}
            <p className="text-sm tabular-nums text-muted-foreground">{coverage.streamOK} of {basis.actions} action streams complete</p>
            <p className={cn('text-sm tabular-nums', coverage.unattributedEvents > 0 ? 'font-medium text-status-failed' : 'text-muted-foreground')}>
              {coverage.unattributedEvents} unattributed events
            </p>
            <p className="text-sm tabular-nums text-muted-foreground" data-testid="decision-latency">{latencyText(basis.decisionLatency)}</p>
            {coverage.maxEvidenceLevel && (
              <p className="text-xs text-muted-foreground">Strongest evidence: {coverage.maxEvidenceLevel}</p>
            )}
            {Object.entries(coverage.excluded ?? {}).some(([, n]) => n > 0) && (
              <div data-testid="coverage-excluded" className="text-sm">
                <p className="text-muted-foreground">Set aside by the monitor, not scored (counted once per Task):</p>
                <ul className="list-disc pl-5 tabular-nums">
                  {Object.entries(coverage.excluded ?? {})
                    .filter(([, n]) => n > 0)
                    .sort(([a], [b]) => a.localeCompare(b))
                    .map(([kind, n]) => <li key={kind}>{excludedText(kind, n)}</li>)}
                </ul>
              </div>
            )}
          </section>

          <section aria-label="Control" className="space-y-1.5 rounded-md border p-3">
            <h3 className="text-sm font-semibold">
              {enforced ? 'Enforced: decisions can hold actions' : 'Audit mode: would pause, never holds'}
            </h3>
            <p className="text-sm tabular-nums text-muted-foreground">{basis.wouldHavePaused} actions would have paused</p>
            <ul className="space-y-0.5 text-sm" aria-label="Charter and concerns">
              {(posture.charterRevisions ?? []).map((rev) => (
                <li key={`c-${rev}`}>
                  {demoCharterRevisions.includes(rev)
                    ? <>Demo charter: preview rule check, audit only <span className="font-mono text-xs text-muted-foreground">{rev}</span></>
                    : <>Charter <span className="font-mono text-xs">{rev}</span> {watchNote}</>}
                </li>
              ))}
              {(posture.concernsRevisions ?? []).map((rev) => (
                <li key={`k-${rev}`}>Concerns <span className="font-mono text-xs">{rev}</span> {watchNote}</li>
              ))}
              {(posture.charterRevisions ?? []).length === 0 && (
                <li className="text-muted-foreground">No charter revision seen in this window</li>
              )}
            </ul>
          </section>

          <section aria-label="Canary basis" className="space-y-1.5 rounded-md border p-3">
            <h3 className="text-sm font-semibold">Canary basis</h3>
            <p className="text-sm text-muted-foreground">
              No canary evidence is reported yet, so no measured miss rate backs these monitors.
            </p>
            <p className="text-xs text-muted-foreground">
              {basis.realProblems} real problems · {basis.falseAlarms} false alarms labelled
            </p>
            {basis.lastObservedAt && (
              <p className="text-xs text-muted-foreground" title={formatTimestamp(basis.lastObservedAt)}>
                Last action {timeAgo(basis.lastObservedAt)}
              </p>
            )}
          </section>
        </div>

        {(blindSpots.length > 0 || posture.visibility.state !== 'full') && (
          <div role="alert" className="space-y-1.5 rounded-md border border-status-failed/50 bg-status-failed-bg p-3" data-testid="coverage-gaps">
            <p className="flex items-center gap-1.5 text-sm font-semibold text-status-failed">
              <AlertTriangle className="size-4" aria-hidden="true" />
              Coverage gaps
            </p>
            <p className="text-sm">{posture.visibility.summary}</p>
            {blindSpots.length > 0 ? (
              <ul className="flex flex-wrap gap-1.5" aria-label="Known blind spots">
                {blindSpots.map(([code, count]) => (
                  <li key={code} className="rounded-md border border-status-failed/40 bg-background px-2 py-0.5 font-mono text-xs">
                    {code} <span className="tabular-nums text-muted-foreground">×{count}</span>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="text-xs text-muted-foreground">No blind spot was declared.</p>
            )}
            {(posture.visibility.reasons?.length ?? 0) > 0 && (
              <ul className="list-inside list-disc text-xs text-muted-foreground">
                {posture.visibility.reasons!.map((reason) => <li key={reason}>{reasonText(reason)}</li>)}
              </ul>
            )}
          </div>
        )}

        {posture.incomplete && (
          <p className="text-sm text-status-failed" role="status">The window held more decisions than were read; counts are a lower bound.</p>
        )}
      </CardContent>
    </Card>
  )
}

function Stat({ label, value }: { label: string; value: number }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="font-semibold tabular-nums">{value.toLocaleString()}</dd>
    </div>
  )
}

/** The 10-second answer: confidence, what was seen, what can be held. */
export function PostureBanner({ window = '24h' }: { window?: string }) {
  const { data, error, isLoading } = useOversightPosture(window)
  const { data: config } = useOversightConfig()
  if (isLoading) return <Skeleton className="h-48 w-full" />
  if (error) {
    if (isNotImplementedError(error)) {
      return (
        <Card>
          <CardContent>
            <EmptyState icon={ShieldAlert} headline="Oversight is not enabled" hint="This controller does not serve oversight decisions." />
          </CardContent>
        </Card>
      )
    }
    return (
      <Card>
        <CardContent>
          <ListAccessError error={error} resource="oversight posture" />
        </CardContent>
      </Card>
    )
  }
  if (!data) return null
  return <PostureView posture={data} demoCharterRevisions={demoRevisions(config)} />
}
