import { cn } from '@/lib/utils'
import { phaseStyle } from '@/lib/task-status'
import type { TaskPhase } from '@/schemas/task'

export interface DistributionSegment {
  phase: TaskPhase | string
  count: number
}

interface DistributionProps {
  segments: DistributionSegment[]
  className?: string
}

/**
 * Horizontal phase-distribution bar — proportional segments colored by the
 * shared status tokens, with a small legend. Each segment's width is its share
 * of the total; an all-zero/empty input degrades to a neutral empty track.
 */
export function Distribution({ segments, className }: DistributionProps) {
  const total = segments.reduce((sum, s) => sum + Math.max(0, s.count), 0)

  return (
    <div className={cn('space-y-2', className)}>
      <div
        className="flex h-2 w-full overflow-hidden rounded-full bg-muted"
        role="img"
        aria-label="Task phase distribution"
      >
        {total > 0 &&
          segments.map((s) => {
            const pct = (Math.max(0, s.count) / total) * 100
            if (pct === 0) return null
            return (
              <div
                key={s.phase}
                data-testid={`dist-segment-${s.phase}`}
                className={cn('h-full', phaseStyle(s.phase).dotClass)}
                style={{ width: `${pct}%` }}
                title={`${phaseStyle(s.phase).label}: ${s.count}`}
              />
            )
          })}
      </div>
      <div className="flex flex-wrap gap-x-4 gap-y-1">
        {segments.map((s) => {
          const style = phaseStyle(s.phase)
          return (
            <span key={s.phase} className="inline-flex items-center gap-1.5 text-xs">
              <span className={cn('size-2 rounded-full', style.dotClass)} aria-hidden="true" />
              <span className="text-muted-foreground">{style.label}</span>
              <span className="font-medium tabular-nums">{s.count}</span>
            </span>
          )
        })}
      </div>
    </div>
  )
}
