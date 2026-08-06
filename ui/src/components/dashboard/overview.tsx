import { useTaskListAll } from '@/hooks/use-tasks'
import { useSessionList } from '@/hooks/use-sessions'
import { useAgentList } from '@/hooks/use-agents'
import { useToolList } from '@/hooks/use-tools'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { PageHeader } from '@/components/layout/page-header'
import { Distribution } from '@/components/ui/distribution'
import { Skeleton } from '@/components/ui/skeleton'
import { taskPhaseSchema } from '@/schemas/task'
import { StatsCards } from './stats-cards'
import { RecentTasks } from './recent-tasks'
import { TaskInventoryError } from '@/components/tasks/task-inventory-error'

// Drive the distribution from the full phase enum (Pending/Running/Succeeded/
// Failed/Scheduled/Cancelled) so every task the Total card counts is also
// represented here — the segment counts always sum to the task total.
const PHASES = taskPhaseSchema.options

export function Overview() {
  const {
    data: tasksData,
    isLoading: tasksLoading,
    error: tasksError,
    isFetching: tasksFetching,
    refetch: refetchTasks,
  } = useTaskListAll('100')
  const { data: sessionsData, isLoading: sessionsLoading } = useSessionList('100')
  const { data: agentsData, isLoading: agentsLoading } = useAgentList()
  const { data: toolsData, isLoading: toolsLoading } = useToolList()

  const isLoading = tasksLoading || sessionsLoading || agentsLoading || toolsLoading

  const tasks = tasksData?.items ?? []
  const distribution = PHASES.map((phase) => ({
    phase,
    count: tasks.filter((t) => (t.status?.phase ?? 'Pending') === phase).length,
  })).filter((seg) => seg.count > 0)

  return (
    <div className="space-y-6">
      <PageHeader title="Dashboard" description="Overview of your Orka workspace" />
      {tasksError ? (
        <TaskInventoryError
          isRetrying={tasksFetching}
          onRetry={() => void refetchTasks()}
        />
      ) : !tasksData ? (
        <div
          role="status"
          aria-label="Loading complete task inventory"
          className="grid gap-6 lg:grid-cols-3"
        >
          <Skeleton className="h-32 w-full lg:col-span-1" />
          <Skeleton className="h-32 w-full lg:col-span-2" />
        </div>
      ) : (
        <>
          <StatsCards
            tasks={tasksData.items}
            sessionCount={sessionsData?.items?.length}
            agentCount={agentsData?.items?.length}
            toolCount={toolsData?.items?.length}
            isLoading={isLoading}
          />
          <div className="grid gap-6 lg:grid-cols-3">
            <Card className="lg:col-span-1">
              <CardHeader>
                <CardTitle className="text-sm font-medium">Phase Distribution</CardTitle>
              </CardHeader>
              <CardContent>
                <Distribution segments={distribution} />
              </CardContent>
            </Card>
            <div className="lg:col-span-2">
              <RecentTasks tasks={tasksData.items} isLoading={tasksLoading} />
            </div>
          </div>
        </>
      )}
    </div>
  )
}
