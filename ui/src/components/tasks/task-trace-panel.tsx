import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState } from '@/components/ui/empty-state'
import { Network, RefreshCw } from 'lucide-react'
import { TaskTraceView } from '@/components/events/task-trace-view'
import { useTaskTrace } from '@/hooks/use-execution-events'
import { ApiError } from '@/lib/api-client'

export function TaskTracePanel({ taskId, taskUid }: { taskId: string; taskUid?: string }) {
  const { data: trace, isLoading, error, refetch, isFetching } = useTaskTrace(taskId, true, taskUid)

  if (isLoading) {
    return (
      <Card>
        <CardContent className="space-y-3 pt-6">
          <Skeleton className="h-6 w-40" />
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
        </CardContent>
      </Card>
    )
  }

  if (error) {
    const notImplemented = error instanceof ApiError && error.status === 501
    return (
      <Card>
        <CardContent className="pt-6">
          <EmptyState
            icon={Network}
            headline={
              notImplemented
                ? 'Execution event storage is not enabled on this server.'
                : 'Failed to load the task trace.'
            }
            hint={notImplemented ? undefined : 'The trace is derived from the task event stream.'}
            action={
              !notImplemented && (
                <Button variant="outline" size="sm" onClick={() => refetch()}>
                  <RefreshCw className="mr-1 h-3 w-3" /> Retry
                </Button>
              )
            }
          />
        </CardContent>
      </Card>
    )
  }

  if (!trace) {
    return (
      <Card>
        <CardContent className="pt-6">
          <EmptyState icon={Network} headline="No trace available for this task yet." />
        </CardContent>
      </Card>
    )
  }

  return (
    <Card>
      <CardContent className="space-y-4 pt-6">
        <div className="flex items-center justify-between">
          <h2 className="text-base font-semibold">Execution trace</h2>
          <Button variant="ghost" size="sm" onClick={() => refetch()} disabled={isFetching}>
            <RefreshCw className={`mr-1 h-3 w-3 ${isFetching ? 'animate-spin' : ''}`} /> Refresh
          </Button>
        </div>
        <TaskTraceView trace={trace} />
      </CardContent>
    </Card>
  )
}
