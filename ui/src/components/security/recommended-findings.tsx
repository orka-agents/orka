import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { ListAccessError } from '@/components/ui/list-access-error'
import { useFindings } from '@/hooks/use-security'
import { isForbiddenError } from '@/lib/api-client'
import { FindingTable } from './finding-table'

export function RecommendedFindings({ repositoryName }: { repositoryName: string }) {
  const { data, isLoading, error } = useFindings(repositoryName, { recommended: 'true', limit: '5' })

  return (
    <Card>
      <CardHeader>
        <CardTitle>Recommended Findings</CardTitle>
      </CardHeader>
      <CardContent>
        {/* A failed load must not read as "no findings"; a later failed poll keeps what loaded. */}
        {error && (isForbiddenError(error) || !data) ? (
          <ListAccessError error={error} resource="recommended findings" />
        ) : isLoading ? (
          <Skeleton className="h-32 w-full" />
        ) : (
          <FindingTable findings={data?.items ?? []} />
        )}
      </CardContent>
    </Card>
  )
}
