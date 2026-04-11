import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { useFindings } from '@/hooks/use-security'
import { FindingTable } from './finding-table'

export function RecommendedFindings({ repositoryName }: { repositoryName: string }) {
  const { data, isLoading } = useFindings(repositoryName, { recommended: 'true', limit: '5' })

  return (
    <Card>
      <CardHeader>
        <CardTitle>Recommended Findings</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? <Skeleton className="h-32 w-full" /> : <FindingTable findings={data?.items ?? []} />}
      </CardContent>
    </Card>
  )
}
