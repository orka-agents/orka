import { Badge } from '@/components/ui/badge'
import type { TaskPhase } from '@/schemas/task'

const phaseStyles: Record<string, string> = {
  Pending: 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-200',
  Running: 'bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200',
  Succeeded: 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200',
  Failed: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
}

export function TaskStatusBadge({ phase }: { phase?: TaskPhase | string }) {
  const p = phase ?? 'Pending'
  return (
    <Badge className={phaseStyles[p] ?? phaseStyles.Pending} variant="secondary">
      {p}
    </Badge>
  )
}
