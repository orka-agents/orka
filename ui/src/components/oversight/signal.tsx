import { cn } from '@/lib/utils'
import { signalPresentation } from '@/lib/oversight'
import { TONE_BADGE } from './tones'

export function SignalBadge({ signal, className }: { signal: string; className?: string }) {
  const presentation = signalPresentation(signal)
  return (
    <span
      data-signal={signal}
      className={cn(
        'inline-flex w-fit shrink-0 items-center gap-1 whitespace-nowrap rounded-full border px-2 py-0.5 text-xs font-medium',
        TONE_BADGE[presentation.tone],
        className,
      )}
    >
      {presentation.label}
    </span>
  )
}

/** A small neutral chip for plain labels. */
export function Chip({ children, className, title }: { children: React.ReactNode; className?: string; title?: string }) {
  return (
    <span title={title} className={cn('inline-flex w-fit max-w-full items-center gap-1 rounded-md border border-border px-1.5 py-0.5 text-xs', className)}>
      {children}
    </span>
  )
}

interface PivotButtonProps {
  /** The search this click runs, as key:value. */
  query: string
  onPivot: (query: string) => void
  children: React.ReactNode
  className?: string
}

/** Any field value that, clicked, searches all time for that value. */
export function PivotButton({ query, onPivot, children, className }: PivotButtonProps) {
  return (
    <button
      type="button"
      onClick={(event) => {
        event.stopPropagation()
        onPivot(query)
      }}
      title={`Search all time for ${query}`}
      aria-label={`Search all time for ${query}`}
      className={cn(
        'inline max-w-full text-left font-mono text-xs break-all underline-offset-2 decoration-dotted hover:underline focus-visible:underline focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none',
        className,
      )}
    >
      {children}
    </button>
  )
}
