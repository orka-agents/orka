import { useTaskList } from '@/hooks/use-tasks'
import { useSessionList } from '@/hooks/use-sessions'
import { useAgentList } from '@/hooks/use-agents'
import { useToolList } from '@/hooks/use-tools'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { PageHeader } from '@/components/layout/page-header'
import { Distribution } from '@/components/ui/distribution'
import { taskPhaseSchema } from '@/schemas/task'
import { StatsCards } from './stats-cards'
import { RecentTasks } from './recent-tasks'

// Drive the distribution from the full phase enum (Pending/Running/Succeeded/
// Failed/Scheduled/Cancelled) so every task the Total card counts is also
// represented here — the segment counts always sum to the task total.
const PHASES = taskPhaseSchema.options

export function Overview() {
  const { data: tasksData, isLoading: tasksLoading } = useTaskList('100')
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
      <StatsCards
        tasks={tasksData?.items}
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
          <RecentTasks tasks={tasksData?.items} isLoading={tasksLoading} />
        </div>
      </div>
    </div>
  )
}
