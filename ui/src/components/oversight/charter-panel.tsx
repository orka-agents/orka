import { ScrollText } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { useOversightConfig, useOversightGroups } from '@/hooks/use-oversight'
import { charterName, isDemoCharter, orderedBounds, queryTerm, type NumberedBound } from '@/lib/oversight'
import { cn } from '@/lib/utils'
import { PivotButton } from './signal'

function BoundCount({ id, window }: { id: string; window: string }) {
  const { data, isLoading, error } = useOversightGroups(queryTerm('bound', id), 'agent', window, true)
  if (isLoading) return <span className="text-xs text-muted-foreground">counting…</span>
  if (error || !data) return <span className="text-xs text-muted-foreground">count unavailable</span>
  const n = data.searched
  return (
    <span className={cn('text-xs tabular-nums', n > 0 ? 'font-medium text-status-failed' : 'text-muted-foreground')} data-testid={`bound-count-${id}`}>
      {data.truncated ? 'at least ' : ''}{n} {n === 1 ? 'action' : 'actions'} broke it in the last {window}
    </span>
  )
}

function BoundItem({ bound, window, onPivot }: { bound: NumberedBound; window: string; onPivot: (q: string) => void }) {
  return (
    <li
      data-highlight={bound.highlight || undefined}
      className={cn('space-y-1 rounded-md border p-2.5', bound.highlight && 'border-foreground/60 bg-accent')}
    >
      <div className="flex flex-wrap items-center gap-1.5 text-xs">
        <span className="font-semibold">Rule {bound.number}</span>
        <PivotButton query={queryTerm('bound', bound.id)} onPivot={onPivot}>{bound.id}</PivotButton>
        {bound.highlight && <span className="rounded-full border border-foreground/40 px-1.5 font-medium">highlighted</span>}
        <BoundCount id={bound.id} window={window} />
      </div>
      <blockquote className="border-l-2 pl-2 text-sm whitespace-pre-wrap break-words">{bound.sentence}</blockquote>
    </li>
  )
}

/** The declared charters and their bounds, quoted as written. Watched, not enforced. */
export function CharterPanel({ window = '24h', onPivot }: { window?: string; onPivot: (q: string) => void }) {
  const { data: config, isLoading, error } = useOversightConfig()
  const charters = config?.charters ?? []
  if (isLoading) return <Skeleton className="h-24 w-full" />
  // The config failing is reported by the rest of the page; this panel stays quiet.
  if (error) return null
  return (
    <Card role="region" aria-label="Charter">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <ScrollText className="size-4" aria-hidden="true" /> Charter
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {charters.length === 0 ? (
          <p className="text-sm text-muted-foreground">No charter declared.</p>
        ) : (
          charters.map((charter) => (
            <section key={charter.revision} aria-label={`Charter ${charterName(charter)}`} className="space-y-2">
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="text-sm font-semibold">{charter.title?.trim() || 'Charter'}</h3>
                <span className="font-mono text-xs text-muted-foreground">{charter.revision}</span>
                <span className="rounded-full border border-status-pending/50 px-2 py-0.5 text-xs text-status-pending">
                  {isDemoCharter(charter, config) ? 'Demo charter: preview rule check, audit only' : 'Watched, not enforced'}
                </span>
              </div>
              {(charter.bounds?.length ?? 0) === 0 ? (
                <p className="text-xs text-muted-foreground">This charter declares no rules.</p>
              ) : (
                <ol className="space-y-2" aria-label={`Rules of ${charterName(charter)}`}>
                  {orderedBounds(charter).map((bound) => (
                    <BoundItem key={bound.id} bound={bound} window={window} onPivot={onPivot} />
                  ))}
                </ol>
              )}
            </section>
          ))
        )}
      </CardContent>
    </Card>
  )
}
