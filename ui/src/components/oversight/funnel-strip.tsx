import { useOversightPosture } from '@/hooks/use-oversight'

export interface NeedYouBreakdown {
  charter: number
  pause: number
  hold: number
}

const plural = (n: number, one: string, many: string) => `${n.toLocaleString()} ${n === 1 ? one : many}`

/**
 * "12,408 kernel events → 314 actions → 3 need you (2 charter breaks ·
 * 0 would-pause · 1 would-hold)": a large count explains itself at a glance.
 * Replayed actions, such as seeded history, are named apart, not counted as live.
 */
export function FunnelStrip({ window, needYou, breakdown }: { window: string; needYou: number; breakdown?: NeedYouBreakdown }) {
  const { data } = useOversightPosture(window)
  if (!data) return null
  const events = data.basis.coverage.observedEvents ?? 0
  const replayed = Math.min(Math.max(data.basis.replayedActions ?? 0, 0), data.basis.actions)
  const live = data.basis.actions - replayed
  const steps = [
    events > 0 ? `${events.toLocaleString()} kernel events` : '',
    `${live.toLocaleString()} ${replayed > 0 ? 'live actions' : 'actions'}`,
    `${needYou.toLocaleString()} need you`,
  ].filter(Boolean)
  const kinds = breakdown
    ? ` (${plural(breakdown.charter, 'charter break', 'charter breaks')} · ${breakdown.pause.toLocaleString()} would-pause · ${breakdown.hold.toLocaleString()} would-hold)`
    : ''
  return (
    <p className="text-sm text-muted-foreground tabular-nums" data-testid="funnel-strip">
      {steps.join(' → ')}
      {kinds}
      {replayed > 0 && <span data-testid="funnel-replayed"> · {plural(replayed, 'replayed action', 'replayed actions')} not counted</span>}
      <span className="sr-only"> in the last {window}</span>
    </p>
  )
}
