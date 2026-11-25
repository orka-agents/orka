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
 */
export function FunnelStrip({ window, needYou, breakdown }: { window: string; needYou: number; breakdown?: NeedYouBreakdown }) {
  const { data } = useOversightPosture(window)
  if (!data) return null
  const events = data.basis.coverage.observedEvents ?? 0
  const steps = [
    events > 0 ? `${events.toLocaleString()} kernel events` : '',
    `${data.basis.actions.toLocaleString()} actions`,
    `${needYou.toLocaleString()} need you`,
  ].filter(Boolean)
  const kinds = breakdown
    ? ` (${plural(breakdown.charter, 'charter break', 'charter breaks')} · ${breakdown.pause.toLocaleString()} would-pause · ${breakdown.hold.toLocaleString()} would-hold)`
    : ''
  return (
    <p className="text-sm text-muted-foreground tabular-nums" data-testid="funnel-strip">
      {steps.join(' → ')}
      {kinds}
      <span className="sr-only"> in the last {window}</span>
    </p>
  )
}
