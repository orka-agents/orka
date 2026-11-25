import { useState } from 'react'
import { Skeleton } from '@/components/ui/skeleton'
import { ListAccessError } from '@/components/ui/list-access-error'
import { useFeedbackOverrides, useOversightConfig, useOversightSimilar, withOverride } from '@/hooks/use-oversight'
import { FEATURE_QUERY_KEY, featureReason, queryTerm, signalPresentation } from '@/lib/oversight'
import { timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import { Chip, PivotButton, SignalBadge } from './signal'
import { SelectionBar } from './selection-bar'

/** Decisions like one decision, each saying exactly which features it shares. */
export function SimilarPanel({ decisionId, onOpenDecision, onPivot }: { decisionId: string; onOpenDecision: (id: string) => void; onPivot: (q: string) => void }) {
  const { data, error, isLoading } = useOversightSimilar(decisionId, true)
  const overrides = useFeedbackOverrides()
  const { data: config } = useOversightConfig()
  const [selected, setSelected] = useState<Set<string>>(new Set())
  if (isLoading) return <Skeleton className="h-24 w-full" />
  if (error) return <ListAccessError error={error} resource="similar decisions" />
  const items = (data?.items ?? []).map((item) => ({ ...item, decision: withOverride(item.decision, overrides) }))
  const toggle = (id: string) => setSelected((prev) => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    return next
  })
  return (
    <div className="space-y-2">
      <p className="text-xs text-muted-foreground" role="status">
        Compared {data?.searched ?? 0} actions{data?.truncated ? ' (more existed; the oldest were not compared)' : ''}. {items.length} alike.
      </p>
      {items.length === 0 ? (
        <p className="text-sm text-muted-foreground">Nothing else shares this action&apos;s behaviour.</p>
      ) : (
        <ul className="space-y-2" aria-label="Similar decisions">
          {items.map(({ decision, score, exactSignature, shared }) => (
            <li
              key={decision.id}
              className={cn('flex gap-2 rounded-md border p-2', signalPresentation(decision.signal).tone === 'muted' && 'text-muted-foreground')}
              data-testid="similar-item"
            >
              <input
                type="checkbox"
                className="mt-1"
                aria-label={`Select ${decision.id}`}
                checked={selected.has(decision.id)}
                onChange={() => toggle(decision.id)}
              />
              <div className="min-w-0 flex-1 space-y-1">
                <div className="flex flex-wrap items-center gap-1.5 text-xs">
                  <span className="font-mono font-semibold tabular-nums">{Math.round(score * 100)}% alike</span>
                  {exactSignature && <Chip className="border-foreground font-medium">exact signature</Chip>}
                  <SignalBadge signal={decision.signal} />
                  <button
                    type="button"
                    className="font-mono underline-offset-2 hover:underline focus-visible:underline focus-visible:outline-none"
                    onClick={() => onOpenDecision(decision.id)}
                  >
                    {decision.action.toolName} · {decision.taskName}
                  </button>
                  <span className="text-muted-foreground">{timeAgo(decision.observedAt)}</span>
                </div>
                {(shared?.length ?? 0) > 0 && (
                  <div className="flex flex-wrap items-center gap-1" aria-label="Why it matched">
                    <span className="text-xs text-muted-foreground">Shares</span>
                    {shared!.map((feature) => {
                      const key = FEATURE_QUERY_KEY[feature.kind]
                      const reason = featureReason(feature, config)
                      return key ? (
                        <PivotButton key={`${feature.kind}:${feature.value}`} query={queryTerm(key, feature.value)} onPivot={onPivot}>{reason}</PivotButton>
                      ) : (
                        <Chip key={`${feature.kind}:${feature.value}`} className="font-mono">{reason}</Chip>
                      )
                    })}
                  </div>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
      <SelectionBar
        selected={items.filter((item) => selected.has(item.decision.id)).map((item) => item.decision)}
        resultSet={items.map((item) => item.decision)}
        query={`like ${decisionId}`}
        onClear={() => setSelected(new Set())}
      />
    </div>
  )
}
