import { Badge } from '@/components/ui/badge'
import { ExternalLink } from 'lucide-react'

interface PRStatusBadgeProps {
  annotations?: Record<string, string>
}

export function PRStatusBadge({ annotations }: PRStatusBadgeProps) {
  if (!annotations) return null

  const prUrl = annotations['orka.ai/pr-url']
  const prNumber = annotations['orka.ai/pr-number']
  const prStatus = annotations['orka.ai/pr-status'] // open, merged, closed

  if (!prUrl && !prNumber) return null

  const statusColors: Record<string, string> = {
    open: 'bg-status-succeeded-bg text-status-succeeded',
    merged: 'bg-type-ai/10 text-type-ai',
    closed: 'bg-status-failed-bg text-status-failed',
  }

  return (
    <a href={prUrl} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1">
      <Badge className={statusColors[prStatus ?? 'open'] ?? statusColors.open} variant="secondary">
        PR #{prNumber ?? '?'} {prStatus ?? 'open'}
        <ExternalLink className="ml-1 h-3 w-3" />
      </Badge>
    </a>
  )
}
