import { cn } from '@/lib/utils'
import { phaseStyle } from '@/lib/task-status'
import type { TaskPhase } from '@/schemas/task'

interface StatusDotProps {
  phase?: TaskPhase | string
  label?: string
  hideLabel?: boolean
  pulse?: boolean
  className?: string
}

export function StatusDot({
  phase,
  label,
  hideLabel = false,
  pulse,
  className,
}: StatusDotProps) {
  const style = phaseStyle(phase)
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
