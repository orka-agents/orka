import { cn } from '@/lib/utils'

interface SparklineProps {
  /** Numeric series to plot, oldest→newest. */
  data: number[]
  width?: number
  height?: number
  /** Stroke color (defaults to currentColor so callers set it via text-*). */
  className?: string
  'aria-label'?: string
}

/**
 * Minimal dependency-free sparkline (inline SVG polyline).
 *
 * Renders a numeric series as a small trend line. Empty or single-point series
 * degrade gracefully to an empty (but valid) SVG rather than throwing.
 */
export function Sparkline({
  data,
  width = 96,
  height = 24,
  className,
  'aria-label': ariaLabel,
}: SparklineProps) {
  const points = (() => {
    if (!data || data.length === 0) return ''
    if (data.length === 1) {
      // A single point: draw a flat midline so the component still renders.
      const y = height / 2
      return `0,${y} ${width},${y}`
    }
    const min = Math.min(...data)
    const max = Math.max(...data)
    const range = max - min || 1
    const stepX = width / (data.length - 1)
    return data
      .map((v, i) => {
        const x = i * stepX
        const y = height - ((v - min) / range) * height
        return `${x.toFixed(2)},${y.toFixed(2)}`
      })
      .join(' ')
  })()

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={cn('text-primary', className)}
      role="img"
      aria-label={ariaLabel ?? 'Trend'}
    >
      {points && (
        <polyline
          points={points}
          fill="none"
          stroke="currentColor"
          strokeWidth={1.5}
          strokeLinecap="round"
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />
      )}
    </svg>
  )
}
