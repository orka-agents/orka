import { useState } from 'react'
import { AlertTriangle, ChevronDown, ChevronRight, History } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { ListAccessError } from '@/components/ui/list-access-error'
import {
  useFeedbackOverrides,
  useOversightConfig,
  useOversightDecision,
  useOversightFeedback,
  useOversightProducers,
  withOverride,
} from '@/hooks/use-oversight'
import { isNotFoundError } from '@/lib/api-client'
import {
  FEEDBACK_LABEL_TEXT,
  MONITOR_FAILURE_STATUSES,
  binaryName,
  charterName,
  concernName,
  destinationDomain,
  ruleName,
  findCharterRule,
  feedbackLabelText,
  isSyntheticText,
  rowBadgeKind,
  rowSummary,
  rowTooltip,
  strongestEvidence,
  formatProbability,
  isMonitorFailure,
  pathDirectoryGlob,
  producerLabel,
  queryTerm,
} from '@/lib/oversight'
import { formatTimestamp, timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import {
  OVERSIGHT_FEEDBACK_LABELS,
  type AdvisorEvaluation,
  type DecisionView,
  type OversightConfig,
  type ProducerInfo,
  type ScorerEvaluation,
} from '@/schemas/oversight'
import { Chip, PivotButton } from './signal'
import { RowBadge } from './decision-table'
import { SimilarPanel } from './similar-panel'

export interface DecisionInspectorProps {
  decisionId: string | undefined
  onClose: () => void
  /** Runs a key:value search across all time. */
  onPivot: (query: string) => void
  /** Opens another decision in this drawer. */
  onOpenDecision: (id: string) => void
  /** A row already loaded, shown while the full decision loads. */
  initial?: DecisionView
  /** The incident's decisions, newest first, when the drawer opened from an incident card. */
  related?: DecisionView[]
}

/** The Action Inspector: a read-only slide-over for one decision. */
export function DecisionInspector({ decisionId, onClose, onPivot, onOpenDecision, initial, related }: DecisionInspectorProps) {
  return (
    <Dialog open={Boolean(decisionId)} onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent
        className="top-0 right-0 left-auto flex h-full max-h-screen w-full max-w-full translate-x-0 translate-y-0 flex-col gap-0 overflow-hidden rounded-none border-l p-0 sm:max-w-2xl"
      >
        {decisionId && (
          <InspectorBody
            key={decisionId}
            decisionId={decisionId}
            onPivot={onPivot}
            onOpenDecision={onOpenDecision}
            initial={initial?.id === decisionId ? initial : undefined}
            related={related?.some((d) => d.id === decisionId) ? related : undefined}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

function InspectorBody({ decisionId, onPivot, onOpenDecision, initial, related }: { decisionId: string; onPivot: (q: string) => void; onOpenDecision: (id: string) => void; initial?: DecisionView; related?: DecisionView[] }) {
  const { data, error, isLoading } = useOversightDecision(decisionId)
  const overrides = useFeedbackOverrides()
  const { data: producersResp } = useOversightProducers()
  const { data: config } = useOversightConfig()
  const producers = producersResp?.producers ?? []
  const [highlighted, setHighlighted] = useState<Set<string>>(new Set())
  const [showSimilar, setShowSimilar] = useState(false)
  const [showDetails, setShowDetails] = useState(false)
  const falseAlarm = useOversightFeedback(decisionId)
  const loaded = data ?? initial
  const decision = loaded ? withOverride(loaded, overrides) : undefined

  const cite = (ids: string[] | undefined) => {
    if (!ids?.length) return
    setHighlighted(new Set(ids))
    const el = document.getElementById(`oversight-event-${ids[0]}`)
    el?.scrollIntoView?.({ block: 'nearest' })
  }

  if (!decision) {
    return (
      <div className="space-y-3 p-6">
        <DialogTitle>Action</DialogTitle>
        <DialogDescription className="sr-only">Oversight decision {decisionId}</DialogDescription>
        {isLoading ? (
          <Skeleton className="h-40 w-full" />
        ) : isNotFoundError(error) ? (
          <p className="text-sm text-muted-foreground">No oversight decision {decisionId} exists in this namespace.</p>
        ) : (
          <ListAccessError error={error} resource="this oversight decision" />
        )}
      </div>
    )
  }

  const { action } = decision
  const effects = action.effects ?? {}
  const pivot = (key: string, value: string) => onPivot(queryTerm(key, value))
  const summary = rowSummary(decision, config)
  const finding = decision.charterFindings?.[0]
  const rule = finding ? findCharterRule(config, decision.charterRevision, finding.boundID) : undefined
  const evidence = strongestEvidence(decision)
  const synthetic = isSyntheticText(decision.actionNarrative)
  const openSimilar = () => {
    setShowSimilar(true)
    // Scroll once the panel has rendered.
    setTimeout(() => document.getElementById('oversight-similar')?.scrollIntoView?.({ block: 'start' }), 0)
  }
  const markFalseAlarm = () => falseAlarm.mutate(
    { label: 'false_alarm' },
    {
      onSuccess: () => toast.success('Marked false alarm'),
      onError: (error) => toast.error(`Could not record the label: ${error instanceof Error ? error.message : 'request failed'}`),
    },
  )

  return (
    <>
      <header className="space-y-2 border-b p-5 pr-12">
        <div className="flex flex-wrap items-center gap-2">
          <RowBadge kind={rowBadgeKind(decision)} />
          {synthetic && <Chip className="text-muted-foreground">synthetic record</Chip>}
        </div>
        <DialogTitle className="text-base leading-snug break-words">{summary}</DialogTitle>
        <DialogDescription className="text-xs">
          {decision.agentName || decision.taskName} · {formatTimestamp(decision.observedAt)}
        </DialogDescription>
      </header>

      <div className="min-h-0 flex-1 space-y-6 overflow-y-auto p-5">
        <section aria-label="Story" className="space-y-3">
          {finding && (
            <div className="space-y-1 rounded-md border border-status-failed/40 bg-status-failed-bg p-3 text-sm">
              <p className="text-xs font-medium" data-testid="charter-rule">
                {rule ? <>Rule {rule.bound.number} of {charterName(rule.charter)}</> : <>Undeclared rule</>}
              </p>
              <blockquote className="border-l-2 border-status-failed pl-2">{rule?.bound.sentence ?? finding.sentence}</blockquote>
              <p className="text-xs text-muted-foreground">{finding.enforced ? 'Enforced.' : 'Audit only: would pause, nothing was held.'}</p>
            </div>
          )}
          <p className="text-sm" data-testid="strongest-evidence">
            {evidence ?? <span className="text-muted-foreground">No kernel evidence: this is inferred from the agent&apos;s request (evidence {decision.evidenceLevel}).</span>}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="outline" onClick={openSimilar}>
              <History aria-hidden="true" /> Has it done this before?
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={markFalseAlarm}
              disabled={falseAlarm.isPending || decision.feedback?.label === 'false_alarm'}
            >
              {decision.feedback?.label === 'false_alarm' ? 'Marked false alarm' : 'Mark false alarm'}
            </Button>
            {decision.feedback && decision.feedback.label !== 'false_alarm' && (
              <span className="text-xs text-muted-foreground">Labelled {feedbackLabelText(decision.feedback.label).toLowerCase()}</span>
            )}
          </div>
        </section>

        {related && related.length > 1 && (
          <Section title={`This incident · ${related.length} actions`}>
            <ol className="space-y-1" aria-label="Actions in this incident">
              {related.map((item) => (
                <li key={item.id}>
                  <button
                    type="button"
                    aria-current={item.id === decision.id ? 'true' : undefined}
                    onClick={() => onOpenDecision(item.id)}
                    title={rowTooltip(item)}
                    className={cn('w-full rounded px-2 py-1 text-left text-xs hover:bg-accent', item.id === decision.id && 'bg-accent font-medium')}
                  >
                    {rowSummary(item, config)} <span className="text-muted-foreground">· {timeAgo(item.observedAt)}</span>
                  </button>
                </li>
              ))}
            </ol>
          </Section>
        )}

        {showSimilar && (
          <div id="oversight-similar">
            <Section title="Has it done this before?">
              <SimilarPanel decisionId={decision.id} onOpenDecision={onOpenDecision} onPivot={onPivot} />
            </Section>
          </div>
        )}

        <div className="border-t pt-3">
          <Button variant="ghost" size="sm" aria-expanded={showDetails} onClick={() => setShowDetails((v) => !v)}>
            {showDetails ? <ChevronDown aria-hidden="true" /> : <ChevronRight aria-hidden="true" />} Details
          </Button>
        </div>

        {showDetails && (
          <div className="space-y-6" data-testid="inspector-details">
            <Section title="Identity">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                <span>task <PivotButton query={queryTerm('task', decision.taskName)} onPivot={onPivot}>{decision.taskName}</PivotButton></span>
                {decision.agentName && (
                  <>
                    <span>agent <PivotButton query={queryTerm('agent', decision.agentName)} onPivot={onPivot}>{decision.agentName}</PivotButton></span>
                    <Button variant="ghost" size="xs" onClick={() => pivot('agent', decision.agentName!)}>
                      This agent&apos;s timeline
                    </Button>
                  </>
                )}
                {decision.workloadIdentity && (
                  <span>identity <PivotButton query={queryTerm('identity', decision.workloadIdentity)} onPivot={onPivot}>{decision.workloadIdentity}</PivotButton></span>
                )}
                {decision.runID && <span>run <PivotButton query={queryTerm('run', decision.runID)} onPivot={onPivot}>{decision.runID}</PivotButton></span>}
              </div>
              <div className="flex flex-wrap gap-1.5 text-xs">
                <Chip className="font-mono">{decision.id}</Chip>
                <Chip>action {decision.actionSeq}</Chip>
                <Chip>disposition {decision.disposition}</Chip>
                <Chip>{decision.mode === 'gate' ? 'gate mode' : 'audit mode'}</Chip>
                <Chip>evidence {decision.evidenceLevel}</Chip>
                {decision.policyRevision && <Chip>policy {decision.policyRevision}</Chip>}
                {decision.replayed && <Chip title="Declared by the producer, like every producer field">replayed (declared by the producer)</Chip>}
                {decision.wouldHavePaused && <Chip>would pause in gate mode</Chip>}
              </div>
            </Section>

            {decision.actionNarrative && (
              <Section title="Recorded narrative">
                {synthetic && (
                  <p className="text-xs text-muted-foreground">Its producer marked this record synthetic: generated for a demo or test, not a real agent action.</p>
                )}
                <p className="text-sm whitespace-pre-wrap break-words">{decision.actionNarrative}</p>
              </Section>
            )}

            <Section title="Tool call">
              <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
                <dt className="text-muted-foreground">Tool</dt>
                <dd><PivotButton query={queryTerm('tool', action.toolName)} onPivot={onPivot}>{action.toolName}</PivotButton></dd>
                {action.toolCallID && (
                  <>
                    <dt className="text-muted-foreground">Call ID</dt>
                    <dd className="font-mono text-xs break-all">{action.toolCallID}</dd>
                  </>
                )}
              </dl>
              {action.requestSummary && (
                <pre data-testid="request-summary" className="mt-2 max-h-48 overflow-auto rounded-md bg-muted p-3 text-xs whitespace-pre-wrap break-words">
                  {action.requestSummary}
                </pre>
              )}
            </Section>

            {(decision.charterFindings?.length ?? 0) > 0 && (
              <Section title="Charter findings">
                <ul className="space-y-2">
                  {decision.charterFindings!.map((finding, i) => (
                    <li key={`${finding.boundID}-${i}`} className="space-y-1 rounded-md border border-status-failed/40 bg-status-failed-bg p-3 text-sm">
                      <div className="flex flex-wrap items-center gap-1.5 text-xs">
                        <span title={finding.sentence}>
                          <PivotButton query={queryTerm('bound', finding.boundID)} onPivot={onPivot}>
                            {ruleName(findCharterRule(config, decision.charterRevision, finding.boundID))}
                          </PivotButton>
                        </span>
                        <Code>{finding.boundID}</Code>
                        <PivotButton query={queryTerm('producer', finding.producerRef)} onPivot={onPivot}>
                          {producerLabel(finding.producerRef, producers)}
                        </PivotButton>
                        <Chip>{finding.enforced ? 'enforced' : 'audit only'}</Chip>
                      </div>
                      <blockquote className="border-l-2 border-status-failed pl-2 italic">{finding.sentence}</blockquote>
                      <CitedEvents ids={finding.citedEventIDs} onCite={cite} />
                    </li>
                  ))}
                </ul>
              </Section>
            )}

            {decision.tier0 && ((decision.tier0.rules?.length ?? 0) > 0 || decision.tier0.highStakesClass) && (
              <Section title="Deterministic rules">
                <div className="flex flex-wrap gap-1.5 text-xs">
                  {decision.tier0.rules?.map((rule) => <Code key={rule}>{rule}</Code>)}
                  {decision.tier0.highStakesClass && <Chip>high-stakes class {decision.tier0.highStakesClass}</Chip>}
                </div>
              </Section>
            )}

            <Section title="Monitor findings">
              {(decision.scorers?.length ?? 0) === 0 && (decision.advisors?.length ?? 0) === 0 ? (
                <p className="text-sm text-muted-foreground">No scorer or advisor evaluated this action.</p>
              ) : (
                <div className="space-y-3">
                  {decision.scorers?.map((scorer, i) => (
                    <ScorerCard key={`s-${scorer.providerRef}-${i}`} scorer={scorer} config={config} producers={producers} onPivot={onPivot} onCite={cite} />
                  ))}
                  {decision.advisors?.map((advisor, i) => (
                    <AdvisorCard key={`a-${advisor.providerRef}-${i}`} advisor={advisor} config={config} producers={producers} onPivot={onPivot} onCite={cite} />
                  ))}
                </div>
              )}
            </Section>

            <Section title="Kernel-observed effects">
              <EffectsSummary decision={decision} onPivot={onPivot} />
              {(effects.notableEvents?.length ?? 0) > 0 && (
                <div className="mt-3 overflow-x-auto">
                  <table className="w-full text-xs" aria-label="Notable events">
                    <thead className="text-left text-muted-foreground">
                      <tr>
                        <th className="py-1 pr-2 font-medium">Event</th>
                        <th className="py-1 pr-2 font-medium">Kind</th>
                        <th className="py-1 pr-2 font-medium">Detail</th>
                        <th className="py-1 pr-2 font-medium">Outcome</th>
                      </tr>
                    </thead>
                    <tbody>
                      {effects.notableEvents!.map((event) => (
                        <tr
                          key={event.id}
                          id={`oversight-event-${event.id}`}
                          data-highlighted={highlighted.has(event.id) || undefined}
                          aria-current={highlighted.has(event.id) ? 'true' : undefined}
                          className={cn('border-t align-top', highlighted.has(event.id) && 'bg-status-pending-bg outline outline-1 outline-status-pending')}
                        >
                          <td className="py-1 pr-2 font-mono">{event.id}{highlighted.has(event.id) && <span className="sr-only"> (cited)</span>}</td>
                          <td className="py-1 pr-2">{event.kind}{event.role && <span className="text-muted-foreground"> · {event.role}</span>}</td>
                          <td className="space-y-0.5 py-1 pr-2">
                            {event.binary && <div><PivotButton query={queryTerm('bin', binaryName(event.binary))} onPivot={onPivot}>{event.binary}</PivotButton></div>}
                            {event.path && <div><PivotButton query={queryTerm('path', event.path)} onPivot={onPivot}>{event.path}</PivotButton></div>}
                            {event.dest && <div><PivotButton query={queryTerm('dest', event.dest)} onPivot={onPivot}>{event.dest}</PivotButton></div>}
                            {event.args && <div className="font-mono break-all text-muted-foreground">{event.args}</div>}
                          </td>
                          <td className="py-1 pr-2">{event.outcome}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                  {(effects.omittedEvents ?? 0) > 0 && (
                    <p className="mt-1 text-xs text-muted-foreground">{effects.omittedEvents} more events were left out of this record.</p>
                  )}
                </div>
              )}
            </Section>

            <Section title="Causal context">
              {(action.priorActionIDs?.length ?? 0) > 0 ? (
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">Prior actions the evaluation saw</p>
                  <ol className="flex flex-wrap gap-1.5">
                    {action.priorActionIDs!.map((id) => (
                      <li key={id}>
                        <Button variant="outline" size="xs" className="font-mono" onClick={() => onOpenDecision(id)}>{id}</Button>
                      </li>
                    ))}
                  </ol>
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">No prior actions were in the evaluation window.</p>
              )}
              <TaintTimeline own={action.taintFlags} inherited={action.inheritedTaint} onPivot={onPivot} />
            </Section>

            <Section title="Coverage">
              <CoverageDetails decision={decision} />
            </Section>

            <Section title="Outcome label">
              <FeedbackControls decision={decision} />
            </Section>

          </div>
        )}
      </div>
    </>
  )
}

/** A machine code, shown only in Details: small and muted beside its name. */
function Code({ children }: { children: React.ReactNode }) {
  return <span className="font-mono text-[10px] text-muted-foreground/80">{children}</span>
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section aria-label={title} className="space-y-2">
      <h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">{title}</h3>
      {children}
    </section>
  )
}

function CitedEvents({ ids, onCite }: { ids?: string[]; onCite: (ids: string[]) => void }) {
  if (!ids?.length) return null
  return (
    <div className="flex flex-wrap items-center gap-1 text-xs">
      <span className="text-muted-foreground">Cites</span>
      {ids.map((id) => (
        <button
          key={id}
          type="button"
          onClick={() => onCite([id])}
          className="rounded border border-border px-1 font-mono hover:bg-accent focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
          aria-label={`Highlight event ${id}`}
        >
          {id}
        </button>
      ))}
    </div>
  )
}

const TRUST_TEXT: Record<string, string> = {
  qualified: 'qualified',
  shadow: 'shadow',
  untrusted_monitor: 'untrusted monitor',
}

function TrustBadge({ state, verb = 'reported' }: { state?: string; verb?: string }) {
  if (!state) return <Chip className="text-muted-foreground">trust not {verb}</Chip>
  return (
    <Chip
      className={cn(state === 'untrusted_monitor' && 'border-status-failed/50 text-status-failed', state === 'shadow' && 'text-muted-foreground')}
      title={`Trust state the producer ${verb}; Orka has not verified it`}
    >
      {TRUST_TEXT[state] ?? state} <span className="text-muted-foreground">({verb})</span>
    </Chip>
  )
}

function ProducerHeader({ role, evaluation, producers, onPivot }: {
  role: string
  evaluation: ScorerEvaluation | AdvisorEvaluation
  producers: ProducerInfo[]
  onPivot: (q: string) => void
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-xs">
      <span className="font-semibold uppercase tracking-wide text-muted-foreground">{role}</span>
      <PivotButton query={queryTerm('producer', evaluation.providerRef)} onPivot={onPivot}>
        {producerLabel(evaluation.providerRef, producers)}
      </PivotButton>
      <Chip className="font-mono">{evaluation.modelID}</Chip>
      {evaluation.modelDigest && <Chip className="font-mono text-muted-foreground" title={evaluation.modelDigest}>{evaluation.modelDigest.slice(0, 19)}</Chip>}
      <TrustBadge state={evaluation.trustState} />
      <Chip className={cn(isMonitorFailure(evaluation.status) && 'border-status-failed text-status-failed')}>status {evaluation.status}</Chip>
      {evaluation.latencyMs !== undefined && <Chip className="tabular-nums text-muted-foreground">{evaluation.latencyMs} ms</Chip>}
    </div>
  )
}

function FailureCallout({ status }: { status: string }) {
  if (!isMonitorFailure(status)) return null
  const known = MONITOR_FAILURE_STATUSES.has(status)
  return (
    <div role="alert" className="flex items-start gap-2 rounded-md border border-dashed border-status-failed p-2 text-xs text-status-failed">
      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
      <span>
        {known ? `No usable answer (${status})` : `Status ${status}`} → would hold. A monitor that cannot answer is never a silent pass.
      </span>
    </div>
  )
}

function ScorerCard({ scorer, config, producers, onPivot, onCite }: { scorer: ScorerEvaluation; config?: OversightConfig; producers: ProducerInfo[]; onPivot: (q: string) => void; onCite: (ids: string[]) => void }) {
  return (
    <div className="space-y-2 rounded-md border p-3" data-testid="scorer-card">
      <ProducerHeader role="Scorer" evaluation={scorer} producers={producers} onPivot={onPivot} />
      <FailureCallout status={scorer.status} />
      {(scorer.scores?.length ?? 0) > 0 && (
        <ul className="space-y-1.5" aria-label="Question scores">
          {scorer.scores!.map((score) => {
            const over = score.threshold !== undefined && score.probability >= score.threshold
            const hot = Boolean(score.triggered) || over
            const pct = Math.max(0, Math.min(1, score.probability)) * 100
            const name = concernName(score.questionID, config, score.label)
            return (
              <li key={score.questionID} className="grid grid-cols-[minmax(4rem,auto)_1fr_auto] items-center gap-2 text-xs" data-triggered={hot || undefined}>
                <span className="min-w-0">
                  <PivotButton query={queryTerm('concern', score.questionID)} onPivot={onPivot} className={cn('font-sans', hot && 'text-status-pending')}>
                    {name}
                  </PivotButton>
                  {name !== score.questionID && <> <Code>{score.questionID}</Code></>}
                </span>
                <div
                  className="relative h-2 rounded-full bg-muted"
                  role="meter"
                  aria-valuemin={0}
                  aria-valuemax={1}
                  aria-valuenow={score.probability}
                  aria-label={`${name} probability`}
                >
                  <div className={cn('h-full rounded-full', hot ? 'bg-status-pending' : 'bg-muted-foreground/40')} style={{ width: `${pct}%` }} />
                  {score.threshold !== undefined && (
                    <div
                      className="absolute -top-0.5 h-3 w-0.5 bg-foreground"
                      style={{ left: `${Math.max(0, Math.min(1, score.threshold)) * 100}%` }}
                      aria-hidden="true"
                    />
                  )}
                </div>
                <span className={cn('font-mono tabular-nums', hot ? 'font-semibold text-status-pending' : 'text-muted-foreground')}>
                  {formatProbability(score.probability)}
                  {score.threshold !== undefined && <> {over ? '≥' : '<'} {formatProbability(score.threshold)}</>}
                  {hot && ' triggered'}
                </span>
              </li>
            )
          })}
        </ul>
      )}
      <CitedEvents ids={scorer.citedEventIDs} onCite={onCite} />
    </div>
  )
}

function AdvisorCard({ advisor, config, producers, onPivot, onCite }: { advisor: AdvisorEvaluation; config?: OversightConfig; producers: ProducerInfo[]; onPivot: (q: string) => void; onCite: (ids: string[]) => void }) {
  return (
    <div className="space-y-2 rounded-md border p-3" data-testid="advisor-card">
      <ProducerHeader role="Advisor" evaluation={advisor} producers={producers} onPivot={onPivot} />
      <FailureCallout status={advisor.status} />
      {advisor.verdict && <p className="text-sm">Verdict: <span className="font-medium">{advisor.verdict}</span></p>}
      {advisor.summary && <p className="text-sm whitespace-pre-wrap break-words text-muted-foreground" data-testid="advisor-summary">{advisor.summary}</p>}
      {(advisor.concerns?.length ?? 0) > 0 && (
        <div className="flex flex-wrap items-center gap-1 text-xs">
          <span className="text-muted-foreground">Concerns</span>
          {advisor.concerns!.map((concern) => {
            const name = concernName(concern, config)
            return (
              <span key={concern}>
                <PivotButton query={queryTerm('concern', concern)} onPivot={onPivot} className="font-sans">{name}</PivotButton>
                {name !== concern && <> <Code>{concern}</Code></>}
              </span>
            )
          })}
        </div>
      )}
      {advisor.releaseRequested && (
        <p className="text-xs text-muted-foreground">
          Release requested · {advisor.releaseGranted ? 'granted' : `not granted${advisor.releaseDeniedReason ? `: ${advisor.releaseDeniedReason}` : ''}`}
        </p>
      )}
      <CitedEvents ids={advisor.citedEventIDs} onCite={onCite} />
    </div>
  )
}

function EffectsSummary({ decision, onPivot }: { decision: DecisionView; onPivot: (q: string) => void }) {
  const effects = decision.action.effects ?? {}
  const counts = Object.entries(effects.counts ?? {}).filter(([, n]) => n > 0)
  const nothing = counts.length === 0 && !effects.binaries?.length && !effects.paths?.length && !effects.destinations?.length
  return (
    <div className="space-y-2 text-sm">
      {nothing && <p className="text-xs text-muted-foreground">No effects were recorded for this action. See Coverage for whether a sensor was watching.</p>}
      {counts.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {counts.map(([kind, n]) => (
            <PivotButton key={kind} query={queryTerm('effect', kind)} onPivot={onPivot}>{kind} ×{n}</PivotButton>
          ))}
        </div>
      )}
      <ValueList label="Binaries" values={effects.binaries} render={(v) => (
        <PivotButton query={queryTerm('bin', binaryName(v))} onPivot={onPivot}>{v}</PivotButton>
      )} />
      <ValueList label="Paths" values={effects.paths} render={(v) => {
        const dir = pathDirectoryGlob(v)
        return (
          <span className="inline-flex flex-wrap gap-1">
            <PivotButton query={queryTerm('path', v)} onPivot={onPivot}>{v}</PivotButton>
            {dir && <PivotButton query={queryTerm('path', dir)} onPivot={onPivot} className="text-muted-foreground">{dir}</PivotButton>}
          </span>
        )
      }} />
      <ValueList label="Destinations" values={effects.destinations} render={(v) => {
        const domain = destinationDomain(v)
        return (
          <span className="inline-flex flex-wrap gap-1">
            <PivotButton query={queryTerm('dest', v)} onPivot={onPivot}>{v}</PivotButton>
            {domain && <PivotButton query={queryTerm('domain', domain)} onPivot={onPivot} className="text-muted-foreground">domain {domain}</PivotButton>}
          </span>
        )
      }} />
      <div className="flex flex-wrap gap-3 text-xs text-muted-foreground">
        {(effects.blockedCount ?? 0) > 0 && <span className="font-medium text-status-failed">{effects.blockedCount} blocked</span>}
        {/* Absent means the sensor did not observe sends, which is not zero. */}
        {effects.bytesSent !== undefined && effects.bytesSent !== null && (
          <span data-testid="bytes-sent">{effects.bytesSent.toLocaleString()} bytes sent (kernel-observed)</span>
        )}
      </div>
    </div>
  )
}

function ValueList({ label, values, render }: { label: string; values?: { value: string; count: number }[]; render: (value: string) => React.ReactNode }) {
  if (!values?.length) return null
  return (
    <div className="space-y-1">
      <p className="text-xs text-muted-foreground">{label}</p>
      <ul className="flex flex-wrap gap-1.5">
        {values.map(({ value, count }) => (
          <li key={value} className="inline-flex items-center gap-1">
            {render(value)}
            {count > 1 && <span className="text-xs tabular-nums text-muted-foreground">×{count}</span>}
          </li>
        ))}
      </ul>
    </div>
  )
}

function TaintTimeline({ own, inherited, onPivot }: { own?: string[]; inherited?: string[]; onPivot: (q: string) => void }) {
  if (!own?.length && !inherited?.length) {
    return <p className="text-xs text-muted-foreground">No taint flags.</p>
  }
  return (
    <ol className="space-y-1 border-l pl-3 text-xs" aria-label="Taint">
      {inherited?.length ? (
        <li>
          <span className="text-muted-foreground">Inherited from the parent Task </span>
          <span className="inline-flex flex-wrap gap-1">
            {inherited.map((flag) => <PivotButton key={`i-${flag}`} query={queryTerm('taint', flag)} onPivot={onPivot}>{flag}</PivotButton>)}
          </span>
        </li>
      ) : null}
      {own?.length ? (
        <li>
          <span className="text-muted-foreground">Accumulated by this Task </span>
          <span className="inline-flex flex-wrap gap-1">
            {own.map((flag) => <PivotButton key={`o-${flag}`} query={queryTerm('taint', flag)} onPivot={onPivot}>{flag}</PivotButton>)}
          </span>
        </li>
      ) : null}
    </ol>
  )
}

export function CoverageDetails({ decision }: { decision: DecisionView }) {
  const coverage = decision.coverage
  const streamOK = coverage.streamState === 'ok'
  const sensorOK = coverage.sensorState === 'events'
  const attribution = Object.entries(coverage.attributionCounts ?? {})
  return (
    <div className="space-y-2 text-sm">
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
        <dt className="text-muted-foreground">Event stream</dt>
        <dd className={cn(!streamOK && 'font-medium text-status-failed')}>{coverage.streamState || 'unknown'}</dd>
        <dt className="text-muted-foreground">Sensor</dt>
        <dd className={cn(!sensorOK && 'font-medium text-status-failed')}>{coverage.sensorState || 'unknown'}</dd>
        {coverage.attributionMethod && (
          <>
            <dt className="text-muted-foreground">Attribution</dt>
            <dd>
              {coverage.attributionMethod}
              {attribution.length > 0 && (
                <span className="text-muted-foreground"> · {attribution.map(([k, n]) => `${k} ${n}`).join(', ')}</span>
              )}
            </dd>
          </>
        )}
        <dt className="text-muted-foreground">Unattributed</dt>
        <dd className={cn((coverage.unattributedEvents ?? 0) > 0 && 'font-medium text-status-failed')}>
          {coverage.unattributedEvents ?? 0} events
        </dd>
      </dl>
      {(coverage.blindSpots?.length ?? 0) > 0 && (
        <div className="rounded-md border border-status-failed/50 bg-status-failed-bg p-2" role="note">
          <p className="text-xs font-semibold text-status-failed">Known blind spots</p>
          <ul className="mt-1 flex flex-wrap gap-1">
            {coverage.blindSpots!.map((spot) => <li key={spot}><Chip className="bg-background font-mono">{spot}</Chip></li>)}
          </ul>
        </div>
      )}
    </div>
  )
}

function FeedbackControls({ decision }: { decision: DecisionView }) {
  const feedback = useOversightFeedback(decision.id)
  const [note, setNote] = useState('')
  const current = decision.feedback
  const submit = (label: (typeof OVERSIGHT_FEEDBACK_LABELS)[number]) => {
    feedback.mutate(
      { label, note: note.trim() || undefined },
      {
        onSuccess: () => {
          setNote('')
          toast.success(`Labelled ${FEEDBACK_LABEL_TEXT[label].toLowerCase()}`)
        },
        onError: (error) => toast.error(`Could not record the label: ${error instanceof Error ? error.message : 'request failed'}`),
      },
    )
  }
  return (
    <div className="space-y-2">
      <p className="text-sm" role="status">
        {current ? (
          <>
            Labelled <span className="font-medium">{feedbackLabelText(current.label)}</span>
            {current.reviewer && <> by <span className="font-mono text-xs">{current.reviewer}</span></>}
            {current.recordedAt && <span className="text-muted-foreground"> · {formatTimestamp(current.recordedAt)}</span>}
            {current.note && <span className="block text-xs text-muted-foreground">Note: {current.note}</span>}
          </>
        ) : (
          <span className="text-muted-foreground">Not labelled yet.</span>
        )}
      </p>
      <Input
        aria-label="Label note (optional)"
        placeholder="Note (optional)"
        value={note}
        maxLength={1000}
        onChange={(event) => setNote(event.target.value)}
      />
      <div className="flex flex-wrap gap-2">
        {OVERSIGHT_FEEDBACK_LABELS.map((label) => (
          <Button
            key={label}
            size="sm"
            variant={current?.label === label ? 'secondary' : 'outline'}
            aria-pressed={current?.label === label}
            disabled={feedback.isPending}
            onClick={() => submit(label)}
          >
            {FEEDBACK_LABEL_TEXT[label]}
          </Button>
        ))}
      </div>
      <p className="text-xs text-muted-foreground">Labels calibrate the monitors; they change nothing an agent does.</p>
    </div>
  )
}
