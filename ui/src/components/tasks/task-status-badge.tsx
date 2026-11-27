import { StatusDot } from '@/components/ui/status-dot'
import type { TaskPhase } from '@/schemas/task'

export function TaskStatusBadge({ phase }: { phase?: TaskPhase | string }) {
  return <StatusDot phase={phase} />
}
