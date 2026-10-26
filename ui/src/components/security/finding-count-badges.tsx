import { Badge } from '@/components/ui/badge'
import type { RepositoryScan } from '@/schemas/security'

type FindingCounts = NonNullable<NonNullable<RepositoryScan['status']>['findingCounts']>

/**
 * Open findings per severity. Critical and high turn red only when there are
 * some; a zero count stays neutral so clean repositories do not read as alarms.
 */
export function FindingCountBadges({ counts }: { counts?: FindingCounts }) {
  const critical = counts?.critical ?? 0
  const high = counts?.high ?? 0
  return (
    <>
      <Badge variant={critical > 0 ? 'destructive' : 'outline'}>{critical} critical</Badge>
      <Badge variant={high > 0 ? 'destructive' : 'outline'}>{high} high</Badge>
      <Badge variant="secondary">{counts?.medium ?? 0} medium</Badge>
      <Badge variant="outline">{counts?.low ?? 0} low</Badge>
    </>
  )
}
