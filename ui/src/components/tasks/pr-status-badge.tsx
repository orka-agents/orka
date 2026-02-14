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
    open: 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200',
    merged: 'bg-purple-100 text-purple-800 dark:bg-purple-900 dark:text-purple-200',
    closed: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
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
