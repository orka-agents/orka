import { Activity, ShieldCheck } from 'lucide-react'
import { EmptyState } from '@/components/ui/empty-state'
import { isForbiddenError } from '@/lib/api-client'

interface ListAccessErrorProps {
  error: unknown
  resource: string
  action?: React.ReactNode
  className?: string
}

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
