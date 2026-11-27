import type { LucideIcon } from 'lucide-react'
import { cn } from '@/lib/utils'

interface EmptyStateProps {
  icon?: LucideIcon
  headline: string
  hint?: string
  action?: React.ReactNode
  className?: string
}

export function EmptyState({
  icon: Icon,
  headline,
  hint,
  action,
  className,
}: EmptyStateProps) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center gap-3 px-6 py-12 text-center',
        className,
      )}
    >
      {Icon && (
        <div className="rounded-full bg-muted p-3 text-muted-foreground">
          <Icon className="size-6" aria-hidden="true" />
        </div>
      )}
      <div className="space-y-1">
        <p className="text-sm font-medium text-foreground">{headline}</p>
        {hint && <p className="max-w-sm text-sm text-muted-foreground">{hint}</p>}
      </div>
      {action && <div className="mt-1">{action}</div>}
    </div>
  )
}
