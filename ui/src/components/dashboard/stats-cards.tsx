import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ListTodo, Play, CheckCircle, XCircle, MessageSquare, Bot, Wrench } from 'lucide-react'
import type { Task } from '@/schemas/task'
import { Skeleton } from '@/components/ui/skeleton'
import { Sparkline } from '@/components/ui/sparkline'
import { phaseStyle } from '@/lib/task-status'
import { cn } from '@/lib/utils'
import type { LoadProblem } from '@/lib/load-problem'

interface StatsCardsProps {
  tasks?: Task[]
  tasksTruncated?: boolean
  tasksProblem?: LoadProblem
  sessionCount?: number
  sessionsTruncated?: boolean
  sessionsProblem?: LoadProblem
  agentCount?: number
  agentsProblem?: LoadProblem
  toolCount?: number
  toolsProblem?: LoadProblem
  isLoading?: boolean
  loading?: { tasks?: boolean; sessions?: boolean; agents?: boolean; tools?: boolean }
}

function ValueSkeleton() {
  return <Skeleton className="h-8 w-16" />
}

function ProblemStat({ resource, problem }: { resource: string; problem: LoadProblem }) {
  const detail = problem.message ? ` (${problem.message})` : ''
  return (
    <div className="space-y-0.5" role="alert">
      <div className="text-sm font-medium">{problem.forbidden ? 'Not authorized' : 'Could not load'}</div>
      <div className="text-xs text-muted-foreground">
        {problem.forbidden
          ? <>Your token lacks <code>{resource}</code> read permission{detail}.</>
          : <>The <code>{resource}</code> request failed{detail}.</>}
      </div>
    </div>
  )
}

function cumulativeSeries(
  tasks: Task[],
  window: { min: number; max: number } | null,
  buckets = 12,
): number[] {
  if (!window) return []
  const times = tasks
    .map((t) => (t.metadata.creationTimestamp ? new Date(t.metadata.creationTimestamp).getTime() : NaN))
    .filter((n) => !Number.isNaN(n))
  const { min, max } = window
  const span = max - min || 1
  const counts = new Array(buckets).fill(0)
  for (const t of times) {
    const idx = Math.min(buckets - 1, Math.max(0, Math.floor(((t - min) / span) * buckets)))
    counts[idx]++
  }
  let acc = 0
  return counts.map((c) => (acc += c))
}

function taskWindow(tasks: Task[]): { min: number; max: number } | null {
  const times = tasks
    .map((t) => (t.metadata.creationTimestamp ? new Date(t.metadata.creationTimestamp).getTime() : NaN))
    .filter((n) => !Number.isNaN(n))
  if (times.length === 0) return null
  return { min: Math.min(...times), max: Math.max(...times) }
}

export function StatsCards({
  tasks,
  tasksTruncated,
  tasksProblem,
  sessionCount,
  sessionsTruncated,
  sessionsProblem,
  agentCount,
  agentsProblem,
  toolCount,
  toolsProblem,
  isLoading,
  loading = {},
}: StatsCardsProps) {
  if (isLoading) {
    return (
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <Card key={i}>
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <Skeleton className="h-4 w-24" />
              <Skeleton className="h-4 w-4" />
            </CardHeader>
            <CardContent>
              <Skeleton className="h-8 w-16" />
            </CardContent>
          </Card>
        ))}
      </div>
    )
  }

  const allTasks = tasks ?? []
  const total = allTasks.length
  const runningTasks = allTasks.filter(t => t.status?.phase === 'Running')
  const succeededTasks = allTasks.filter(t => t.status?.phase === 'Succeeded')
  const failedTasks = allTasks.filter(t => t.status?.phase === 'Failed')
  const running = runningTasks.length
  const succeeded = succeededTasks.length
  const failed = failedTasks.length

  const finished = succeeded + failed
  const successRate = finished > 0 ? Math.round((succeeded / finished) * 100) : null

  const window = taskWindow(allTasks)
  const stats = [
    { label: tasksTruncated ? 'Tasks loaded' : 'Total Tasks', value: total, icon: ListTodo, color: 'text-foreground', spark: 'text-primary', series: cumulativeSeries(allTasks, window) },
    { label: 'Running', value: running, icon: Play, color: phaseStyle('Running').textClass, spark: phaseStyle('Running').textClass, series: cumulativeSeries(runningTasks, window) },
    {
      label: 'Succeeded',
      value: succeeded,
      icon: CheckCircle,
      color: phaseStyle('Succeeded').textClass,
      spark: phaseStyle('Succeeded').textClass,
      sub: successRate !== null
        ? (tasksTruncated ? `${successRate}% of loaded finished tasks` : `${successRate}% success rate`)
        : undefined,
      series: cumulativeSeries(succeededTasks, window),
    },
    { label: 'Failed', value: failed, icon: XCircle, color: phaseStyle('Failed').textClass, spark: phaseStyle('Failed').textClass, series: cumulativeSeries(failedTasks, window) },
  ]

  return (
    <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
      {stats.map(({ label, value, icon: Icon, color, spark, sub, series }) => (
        <Card key={label}>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">{label}</CardTitle>
            <Icon className={`h-4 w-4 ${color}`} />
          </CardHeader>
          <CardContent>
            {loading.tasks ? (
              <ValueSkeleton />
            ) : tasksProblem ? (
              <ProblemStat resource="tasks" problem={tasksProblem} />
            ) : (
              <div className="flex items-end justify-between gap-2">
                <div>
                  <div className="text-2xl font-bold tabular-nums">{value}</div>
                  {sub && <div className="text-xs text-muted-foreground">{sub}</div>}
                </div>
                {series.length > 1 && (
                  <Sparkline data={series} className={cn('opacity-80', spark)} aria-label={`${label} trend`} />
                )}
              </div>
            )}
          </CardContent>
        </Card>
      ))}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium">{sessionsTruncated ? 'Sessions loaded' : 'Sessions'}</CardTitle>
          <MessageSquare className="h-4 w-4 text-muted-foreground" />
        </CardHeader>
        <CardContent>
          {loading.sessions ? (
            <ValueSkeleton />
          ) : sessionsProblem ? (
            <ProblemStat resource="sessions" problem={sessionsProblem} />
          ) : (
            <div className="text-2xl font-bold tabular-nums">{sessionCount ?? 0}</div>
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium">Agents</CardTitle>
          <Bot className="h-4 w-4 text-muted-foreground" />
        </CardHeader>
        <CardContent>
          {loading.agents ? (
            <ValueSkeleton />
          ) : agentsProblem ? (
            <ProblemStat resource="agents" problem={agentsProblem} />
          ) : (
            <div className="text-2xl font-bold tabular-nums">{agentCount ?? 0}</div>
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium">Tools</CardTitle>
          <Wrench className="h-4 w-4 text-muted-foreground" />
        </CardHeader>
        <CardContent>
          {loading.tools ? (
            <ValueSkeleton />
          ) : toolsProblem ? (
            <ProblemStat resource="tools" problem={toolsProblem} />
          ) : (
            <div className="text-2xl font-bold tabular-nums">{toolCount ?? 0}</div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
