import { Badge } from '@/components/ui/badge'
import { ExternalLink } from 'lucide-react'
import type { TaskDeliveryStatus } from '@/schemas/task'

type PullRequestReceipt = NonNullable<TaskDeliveryStatus['prReceipt']>

interface PRStatusBadgeProps {
  /** The Task's `status.delivery.prReceipt`, written when publication opens a pull request. */
  receipt?: PullRequestReceipt
}

// The controller records the forge state as Open, Closed or Merged.
const statusColors: Record<string, string> = {
  open: 'bg-status-succeeded-bg text-status-succeeded',
  merged: 'bg-type-ai/10 text-type-ai',
  closed: 'bg-status-failed-bg text-status-failed',
}

/** The receipt URL as a link target, only when it is plain http(s). */
function safeHref(url?: string): string | undefined {
  if (!url) return undefined
  try {
    const parsed = new URL(url)
    return parsed.protocol === 'https:' || parsed.protocol === 'http:' ? parsed.href : undefined
  } catch {
    return undefined
  }
}

export function PRStatusBadge({ receipt }: PRStatusBadgeProps) {
  if (!receipt) return null

  const state = receipt.state?.toLowerCase()
  const label = ['PR', receipt.number ? `#${receipt.number}` : undefined, state].filter(Boolean).join(' ')
  const href = safeHref(receipt.url)
  const badge = (
    <Badge className={statusColors[state ?? ''] ?? 'bg-muted text-muted-foreground'} variant="secondary">
      {label}
      {href && <ExternalLink className="ml-1 h-3 w-3" />}
    </Badge>
  )
  if (!href) return badge
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1">
      {badge}
    </a>
  )
}
