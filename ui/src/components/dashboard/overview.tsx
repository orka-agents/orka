import { useTaskList } from '@/hooks/use-tasks'
import { useSessionList } from '@/hooks/use-sessions'
import { useAgentList } from '@/hooks/use-agents'
import { useToolList } from '@/hooks/use-tools'
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
      <div>
        <h1 className="text-3xl font-bold tracking-tight">Dashboard</h1>
        <p className="text-muted-foreground">Overview of your Mercan workspace</p>
      </div>
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
