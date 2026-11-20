import { Activity, ShieldCheck } from 'lucide-react'
import { EmptyState } from '@/components/ui/empty-state'
import { isForbiddenError } from '@/lib/api-client'

interface ListAccessErrorProps {
  error: unknown
  /** Plural, human-readable resource label, e.g. "tasks". */
  resource: string
  /** Optional way onward, e.g. a link back to the list page. */
  action?: React.ReactNode
  className?: string
}

/**
 * Error placeholder for list views. A 403 must never fall through to the
 * "nothing here, create one" empty state: the token cannot see the list, so
 * the view says so instead of inviting the user to create resources.
 */
export function ListAccessError({ error, resource, action, className }: ListAccessErrorProps) {
  const forbidden = isForbiddenError(error)
  const message = error instanceof Error ? error.message.trim() : ''
  const detail = message ? ` (${message})` : ''
  return (
    <div role="alert" className={className}>
      <EmptyState
        icon={forbidden ? ShieldCheck : Activity}
        headline={forbidden ? `Not authorized to view ${resource}` : `Could not load ${resource}`}
        hint={forbidden
          ? `Your token lacks read permission for ${resource}${detail}.`
          : message || `The ${resource} request failed.`}
        action={action}
      />
    </div>
  )
}
