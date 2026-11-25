import { useOversightPosture } from '@/hooks/use-oversight'

/** "12,408 kernel events → 314 actions → 3 need you". */
export function FunnelStrip({ window, needYou }: { window: string; needYou: number }) {
  const { data } = useOversightPosture(window)
  if (!data) return null
  const events = data.basis.coverage.observedEvents ?? 0
  const steps = [
    events > 0 ? `${events.toLocaleString()} kernel events` : '',
    `${data.basis.actions.toLocaleString()} actions`,
    `${needYou.toLocaleString()} need you`,
  ].filter(Boolean)
  return (
    <p className="text-sm text-muted-foreground tabular-nums" data-testid="funnel-strip">
      {steps.join(' → ')}
      <span className="sr-only"> in the last {window}</span>
    </p>
  )
}
