import { cn } from '@/lib/utils'
import { useDocumentTitle } from '@/hooks/use-document-title'

interface PageHeaderProps {
  title: string
  description?: string
  eyebrow?: string
  action?: React.ReactNode
  className?: string
}

export function PageHeader({
  title,
  description,
  eyebrow,
  action,
  className,
}: PageHeaderProps) {
  useDocumentTitle(title)
  return (
    <div
      className={cn(
        'flex min-w-0 flex-col gap-2 sm:flex-row sm:items-center sm:justify-between',
        className,
      )}
    >
      <div className="min-w-0 space-y-1">
        {eyebrow && (
          <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {eyebrow}
          </p>
        )}
        <h1 className="break-words text-3xl font-bold tracking-tight">{title}</h1>
        {description && <p className="text-muted-foreground">{description}</p>}
      </div>
      {action && <div className="flex shrink-0 items-center gap-2">{action}</div>}
    </div>
  )
}
