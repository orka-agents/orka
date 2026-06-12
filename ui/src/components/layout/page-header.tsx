import { cn } from '@/lib/utils'

interface PageHeaderProps {
  /** Primary page title (rendered as an h1). */
  title: string
  /** Optional supporting description below the title. */
  description?: string
  /** Optional small uppercase label above the title (e.g. a section/context). */
  eyebrow?: string
  /** Optional action slot rendered on the trailing edge (e.g. a "New" button). */
  action?: React.ReactNode
  className?: string
}

/**
 * Standard page header — title + optional description, with an optional action
 * slot on the trailing edge.
 *
 * Replaces the `<h1 className="text-3xl font-bold tracking-tight"> + muted <p>`
 * pattern that was duplicated across ~20 page/list components, so the page
 * chrome stays consistent and is changed in exactly one place.
 */
export function PageHeader({
  title,
  description,
  eyebrow,
  action,
  className,
}: PageHeaderProps) {
  return (
    <div
      className={cn(
        'flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between',
        className,
      )}
    >
      <div className="space-y-1">
        {eyebrow && (
          <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {eyebrow}
          </p>
        )}
        <h1 className="text-3xl font-bold tracking-tight">{title}</h1>
        {description && <p className="text-muted-foreground">{description}</p>}
      </div>
      {action && <div className="flex shrink-0 items-center gap-2">{action}</div>}
    </div>
  )
}
