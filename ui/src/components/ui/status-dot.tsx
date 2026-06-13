import { cn } from '@/lib/utils'
import { phaseStyle } from '@/lib/task-status'
import type { TaskPhase } from '@/schemas/task'

interface StatusDotProps {
  /** Task phase; drives the dot color and label. */
  phase?: TaskPhase | string
  /** Override the rendered label (defaults to the phase label). */
  label?: string
  /** Hide the visible text label (a screen-reader label is still emitted). */
  hideLabel?: boolean
  /**
   * Force the "breathing" liveness pulse. When omitted, the pulse is applied
   * automatically for live phases (Running). Motion is gated behind
   * `motion-safe:` so reduced-motion users see a still dot.
   */
  pulse?: boolean
  className?: string
}

/**
 * The canonical status indicator: a filled circle in the phase color plus a
 * label.
 *
 * Color is never the only signal — a visible label accompanies the dot, and
 * when the label is hidden an `sr-only` text alternative is emitted so the
 * phase is still conveyed to assistive tech. The dot itself is decorative
 * (`aria-hidden`). Liveness is conveyed by both hue and a motion-safe pulse.
 */
export function StatusDot({
  phase,
  label,
  hideLabel = false,
  pulse,
  className,
}: StatusDotProps) {
  const style = phaseStyle(phase)
  // Preserve the caller's phase text verbatim (truthful for unknown/custom
  // backend phases); only the *styling* falls back to Pending. An explicit
  // `label` always wins; an absent phase falls back to the style label.
  const text = label ?? (typeof phase === 'string' && phase ? phase : style.label)
  const isLive = pulse ?? style.live

  return (
    <span className={cn('inline-flex items-center gap-1.5', className)}>
      <span
        data-testid="status-dot"
        data-phase={style.label}
        aria-hidden="true"
        className={cn(
          'inline-block size-2 shrink-0 rounded-full',
          style.dotClass,
          isLive && 'motion-safe:animate-pulse-live',
        )}
      />
      {hideLabel ? (
        <span className="sr-only">{text}</span>
      ) : (
        <span className={cn('text-xs font-medium', style.textClass)}>{text}</span>
      )}
    </span>
  )
}
