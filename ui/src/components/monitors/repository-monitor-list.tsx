import { Link } from '@tanstack/react-router'
import { Activity, GitPullRequest, Play, Plus, Wrench } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { ListAccessError } from '@/components/ui/list-access-error'
import { PageHeader } from '@/components/layout/page-header'
import { useRepositoryMonitors, useRunRepositoryMonitor } from '@/hooks/use-monitors'
import type { RepositoryMonitor } from '@/schemas/monitor'
import { statusProblem } from '@/components/common/status-problem'
import { repositoryMonitorDisplayName } from './repository-monitor-display'
import { timeAgo } from '@/lib/time'

export function RepositoryMonitorList() {
  const { data, isLoading, error } = useRepositoryMonitors()
  const monitors = error ? [] : (data?.items ?? [])

  return (
    <div className="space-y-4">
      <PageHeader
        title="Repository Monitors"
        description="Maintainer automation state for pull requests, repairs, and audit trails"
        action={
          <Link to="/monitors/create/new">
            <Button>
              <Plus className="mr-2 h-4 w-4" />
              New Monitor
            </Button>
          </Link>
        }
      />

      {isLoading ? (
        <div className="grid gap-4 md:grid-cols-2">
          {Array.from({ length: 4 }).map((_, index) => (
            <Card key={index}>
              <CardHeader><Skeleton className="h-6 w-48" /></CardHeader>
              <CardContent className="space-y-2">
                <Skeleton className="h-4 w-full" />
                <Skeleton className="h-4 w-2/3" />
              </CardContent>
            </Card>
          ))}
        </div>
      ) : error ? (
        <Card><CardContent><ListAccessError error={error} resource="repository monitors" /></CardContent></Card>
      ) : monitors.length === 0 ? (
        <Card>
          <CardContent className="space-y-4 py-12 text-center">
            <div className="space-y-1">
              <p className="font-medium">No repository monitors configured.</p>
              <p className="text-sm text-muted-foreground">Create a monitor to review GitHub pull requests from the dashboard.</p>
            </div>
            <Link to="/monitors/create/new">
              <Button>
                <Plus className="mr-2 h-4 w-4" />
                Create repository monitor
              </Button>
            </Link>
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {monitors.map((monitor) => (
            <RepositoryMonitorCard key={monitor.metadata.name} monitor={monitor} />
          ))}
        </div>
      )}
    </div>
  )
}

function RepositoryMonitorCard({ monitor }: { monitor: RepositoryMonitor }) {
  const runMonitor = useRunRepositoryMonitor(monitor.metadata.name)
  const status = monitor.status
  const displayName = repositoryMonitorDisplayName(monitor)
  const problem = statusProblem(status)
  const run = () => runMonitor.mutate(undefined, {
    onSuccess: () => toast.success(`Run of ${monitor.metadata.name} started`),
    onError: (error) => toast.error(`Failed to start monitor run: ${error instanceof Error ? error.message : 'Unknown error'}`),
  })

  return (
    <Card className="transition-colors hover:border-primary/50">
      <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-lg">
            <Activity className="h-5 w-5 shrink-0 text-primary" />
            <Link to="/monitors/$monitorId" params={{ monitorId: monitor.metadata.name }} className="hover:underline">
              {monitor.metadata.name}
            </Link>
          </CardTitle>
          <p className="text-sm text-muted-foreground">{displayName} · {monitor.spec.branch || 'main'} · {monitor.spec.schedule || 'manual'}</p>
          {problem && <p className="text-sm font-medium text-status-failed" title={problem.message}>{problem.reason}</p>}
        </div>
        <Badge variant={status?.phase === 'Ready' ? 'default' : 'secondary'}>
          {status?.phase || 'Pending'}
        </Badge>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="text-sm text-muted-foreground">{monitor.spec.repoURL}</div>
        <div className="grid gap-2 text-sm md:grid-cols-2">
          <div className="flex items-center gap-2">
            <GitPullRequest className="h-4 w-4 text-muted-foreground" />
            <span>Pending reviews: <span className="font-medium text-foreground">{status?.pendingReviews ?? 0}</span></span>
          </div>
          <div className="flex items-center gap-2">
            <Wrench className="h-4 w-4 text-muted-foreground" />
            <span>Active repairs: <span className="font-medium text-foreground">{status?.activeRepairs ?? 0}</span></span>
          </div>
          <div>Open PRs: <span className="font-medium text-foreground">{status?.openPullRequests ?? 0}</span></div>
          <div>Last run: <span className="font-medium text-foreground">{timeAgo(status?.lastRunTime ?? status?.lastSuccessfulRunTime, { empty: 'Never' })}</span></div>
        </div>
        <div className="flex items-center justify-between">
          <Link to="/monitors/$monitorId" params={{ monitorId: monitor.metadata.name }}>
            <Button variant="outline">Open</Button>
          </Link>
          <Button variant="secondary" onClick={run} disabled={runMonitor.isPending}>
            <Play className="mr-2 h-4 w-4" />
            Run
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
