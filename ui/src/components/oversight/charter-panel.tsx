import { ScrollText } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { useOversightConfig, useOversightGroups } from '@/hooks/use-oversight'
import { boundQuery, charterName, isDemoCharter, orderedBounds, type NumberedBound } from '@/lib/oversight'
import { cn } from '@/lib/utils'

function BoundCount({ id, revision, number, window, onOpen }: { id: string; revision: string; number: number; window: string; onOpen: (q: string) => void }) {
  const q = boundQuery(id, revision)
  const { data, isLoading, error } = useOversightGroups(q, 'agent', window, true)
  if (isLoading) return <span className="text-xs text-muted-foreground">counting…</span>
  if (error || !data) return <span className="text-xs text-muted-foreground">count unavailable</span>
  const n = data.searched
  return (
    <button
      type="button"
      onClick={() => onOpen(q)}
      aria-label={`Show the ${data.truncated ? 'at least ' : ''}${n} ${n === 1 ? 'action' : 'actions'} that broke rule ${number} in the last ${window}`}
      className={cn('text-xs tabular-nums underline-offset-2 decoration-dotted hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none', n > 0 ? 'font-medium text-status-failed' : 'text-muted-foreground')}
      data-testid={`bound-count-${id}`}
    >
      {data.truncated ? 'at least ' : ''}{n} {n === 1 ? 'action' : 'actions'} broke it in the last {window}
    </button>
  )
}

function BoundItem({ bound, revision, window, onOpenBound }: { bound: NumberedBound; revision: string; window: string; onOpenBound: (q: string) => void }) {
  return (
    <li
      data-highlight={bound.highlight || undefined}
      className={cn('space-y-1 rounded-md border p-2.5', bound.highlight && 'border-foreground/60 bg-accent')}
    >
      <div className="flex flex-wrap items-center gap-1.5 text-xs">
        <span className="font-semibold" title={bound.sentence}>Rule {bound.number}</span>
        {bound.highlight && <span className="rounded-full border border-foreground/40 px-1.5 font-medium">highlighted</span>}
        <BoundCount id={bound.id} revision={revision} number={bound.number} window={window} onOpen={onOpenBound} />
      </div>
      <blockquote className="border-l-2 pl-2 text-sm whitespace-pre-wrap break-words">{bound.sentence}</blockquote>
    </li>
  )
}

/** The declared charters and their bounds, quoted as written. Watched, not enforced. */
export function CharterPanel({ window = '24h', onPivot, onOpenBound }: {
  window?: string
  onPivot: (q: string) => void
  /** Opens the actions that broke one bound; defaults to an all-time search. */
  /** Opens the actions that broke a rule, given the rule's search. */
  onOpenBound?: (q: string) => void
}) {
  const openBound = onOpenBound ?? onPivot
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
                <span className="text-xs text-muted-foreground">audit mode · {charter.bounds?.length ?? 0} rules</span>
              </div>
              {(charter.bounds?.length ?? 0) === 0 ? (
                <p className="text-xs text-muted-foreground">This charter declares no rules.</p>
              ) : (
                <ol className="space-y-2" aria-label={`Rules of ${charterName(charter)}`}>
                  {orderedBounds(charter).map((bound) => (
                    <BoundItem key={bound.id} bound={bound} revision={charter.revision} window={window} onOpenBound={openBound} />
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
