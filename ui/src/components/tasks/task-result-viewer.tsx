import { useTaskResult } from '@/hooks/use-tasks'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'

export function TaskResultViewer({ taskId }: { taskId: string }) {
  const { data, isLoading, refetch, isFetched } = useTaskResult(taskId)

  if (!isFetched) {
    return (
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle>Result</CardTitle>
          <Button variant="outline" size="sm" onClick={() => refetch()}>Load Result</Button>
        </CardHeader>
      </Card>
    )
  }

  if (isLoading) {
    return (
      <Card>
        <CardHeader><CardTitle>Result</CardTitle></CardHeader>
        <CardContent><Skeleton className="h-32 w-full" /></CardContent>
      </Card>
    )
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Result</CardTitle>
      </CardHeader>
      <CardContent>
        <pre className="max-h-96 overflow-auto rounded-md bg-muted p-4 text-sm whitespace-pre-wrap">
          {data?.result ?? 'No result available'}
        </pre>
      </CardContent>
    </Card>
  )
}
