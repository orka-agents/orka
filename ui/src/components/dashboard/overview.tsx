import { useTaskList } from '@/hooks/use-tasks'
import { useSessionList } from '@/hooks/use-sessions'
import { useAgentList } from '@/hooks/use-agents'
import { useToolList } from '@/hooks/use-tools'
import { PageHeader } from '@/components/layout/page-header'
import { StatsCards } from './stats-cards'
import { RecentTasks } from './recent-tasks'

export function Overview() {
  const { data: tasksData, isLoading: tasksLoading } = useTaskList('100')
  const { data: sessionsData, isLoading: sessionsLoading } = useSessionList('100')
  const { data: agentsData, isLoading: agentsLoading } = useAgentList()
  const { data: toolsData, isLoading: toolsLoading } = useToolList()

  const isLoading = tasksLoading || sessionsLoading || agentsLoading || toolsLoading

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
      <RecentTasks tasks={tasksData?.items} isLoading={tasksLoading} />
    </div>
  )
}
