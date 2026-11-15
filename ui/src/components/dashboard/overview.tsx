import { Link } from '@tanstack/react-router'
import { useTaskListAll } from '@/hooks/use-tasks'
import { useSessionListAll } from '@/hooks/use-sessions'
import { useAgentListAll } from '@/hooks/use-agents'
import { useToolListAll } from '@/hooks/use-tools'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { PageHeader } from '@/components/layout/page-header'
import { Distribution } from '@/components/ui/distribution'
import { taskPhaseSchema } from '@/schemas/task'
import { StatsCards } from './stats-cards'
import { RecentTasks } from './recent-tasks'
import { loadProblem, loadProblemText } from '@/lib/load-problem'

// Drive the distribution from the full phase enum (Pending/Running/Succeeded/
// Failed/Scheduled/Cancelled) so every task the Total card counts is also
// represented here — the segment counts always sum to the task total.
const PHASES = taskPhaseSchema.options

export function Overview() {
  // The dashboard is a glanceable summary: it polls the bounded list walks
  // at a relaxed cadence instead of hammering full-history queries.
  const { data: tasksData, isLoading: tasksLoading, error: tasksError } = useTaskListAll('100', 60000)
  const { data: sessionsData, isLoading: sessionsLoading, error: sessionsError } = useSessionListAll('100', 60000)
  const { data: agentsData, isLoading: agentsLoading, error: agentsError } = useAgentListAll()
  const { data: toolsData, isLoading: toolsLoading, error: toolsError } = useToolListAll()

  // A failed list leaves `data` undefined; surface it per resource instead
  // of letting the missing collection render as a fabricated zero.
  const tasksProblem = loadProblem(tasksError, tasksData !== undefined)

  const tasks = tasksData?.items ?? []
  const tasksTruncated = tasksData?.truncated ?? Boolean(tasksData?.metadata?.continue)
  const sessionsTruncated = sessionsData?.truncated ?? Boolean(sessionsData?.metadata?.continue)
  const distribution = PHASES.map((phase) => ({
    phase,
    count: tasks.filter((t) => (t.status?.phase ?? 'Pending') === phase).length,
  })).filter((seg) => seg.count > 0)

  return (
    <div className="space-y-6">
      <PageHeader title="Dashboard" description="Overview of your Orka workspace" action={<Link to="/usage" className="text-sm text-primary underline">Usage and PR outcomes</Link>} />
      {tasksTruncated && !tasksProblem && (
        <p className="text-sm text-muted-foreground" role="status">
          Task counts and phase distribution use {tasks.length.toLocaleString()} loaded tasks in resource-key order. More tasks exist.
        </p>
      )}
      <StatsCards
        tasks={tasksData?.items}
        tasksTruncated={tasksTruncated}
        tasksProblem={tasksProblem}
        sessionCount={sessionsData?.items?.length}
        sessionsTruncated={sessionsTruncated}
        sessionsProblem={loadProblem(sessionsError, sessionsData !== undefined)}
        agentCount={agentsData?.items?.length}
        agentsProblem={loadProblem(agentsError, agentsData !== undefined)}
        toolCount={toolsData?.items?.length}
        toolsProblem={loadProblem(toolsError, toolsData !== undefined)}
        // Each card renders as soon as its own list arrives.
        loading={{ tasks: tasksLoading, sessions: sessionsLoading, agents: agentsLoading, tools: toolsLoading }}
      />
      <div className="grid gap-6 lg:grid-cols-3">
        {/* Sized to its content instead of stretching to the Recent Tasks height. */}
        <Card className="lg:col-span-1 lg:self-start">
          <CardHeader>
            <CardTitle className="text-sm font-medium">
              {tasksTruncated ? 'Loaded Phase Distribution' : 'Phase Distribution'}
            </CardTitle>
          </CardHeader>
          <CardContent>
            {tasksProblem ? (
              <p className="text-sm text-muted-foreground" role="alert">
                {loadProblemText('tasks', tasksProblem)}
              </p>
            ) : (
              <Distribution segments={distribution} />
            )}
          </CardContent>
        </Card>
        <div className="lg:col-span-2">
          <RecentTasks
            tasks={tasksProblem ? undefined : tasksData?.items}
            isLoading={tasksLoading}
            problem={tasksProblem}
            isTruncated={tasksTruncated}
          />
        </div>
      </div>
    </div>
  )
}
