import { StatusDot } from '@/components/ui/status-dot'
import type { TaskPhase } from '@/schemas/task'

/**
 * Phase badge for a task. Thin wrapper over the shared <StatusDot>, which
 * sources all phase color/label from `lib/task-status.ts` (the single source
 * of truth) — no local color map.
 */
export function TaskStatusBadge({ phase }: { phase?: TaskPhase | string }) {
  // StatusDot renders the literal phase text (so an unknown phase still reads
  // as itself) while its dot color falls back to Pending via the shared map.
  return <StatusDot phase={phase} />
}
