import { Play } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useRepositoryMonitor, useRepositoryMonitorItems, useRepositoryMonitorRuns, useRunRepositoryMonitor } from '@/hooks/use-monitors'
import { repositoryMonitorDisplayName } from './repository-monitor-display'

function shortSHA(value?: string) {
  if (!value) return '-'
  return value.slice(0, 8)
}

function formatTime(value?: string) {
  if (!value) return 'Never'
  return new Date(value).toLocaleString()
}

export function RepositoryMonitorDetail({ monitorName }: { monitorName: string }) {
  const { data: monitor, isLoading } = useRepositoryMonitor(monitorName)
  const runs = useRepositoryMonitorRuns(monitorName)
  const items = useRepositoryMonitorItems(monitorName)
  const runMonitor = useRunRepositoryMonitor(monitorName)

  if (isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-10 w-80" />
        <Skeleton className="h-32 w-full" />
      </div>
    )
  }

  if (!monitor) {
    return <div className="text-muted-foreground">Repository monitor not found.</div>
  }

  const status = monitor.status
  const displayName = repositoryMonitorDisplayName(monitor)

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">{displayName}</h1>
          <p className="text-muted-foreground">{monitor.spec.repoURL}</p>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant={status?.phase === 'Ready' ? 'default' : 'secondary'}>{status?.phase || 'Pending'}</Badge>
          <Button variant="secondary" onClick={() => runMonitor.mutate()} disabled={runMonitor.isPending}>
            <Play className="mr-2 h-4 w-4" />
            Run
          </Button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-5">
        <MetricCard title="Open PRs" value={status?.openPullRequests ?? 0} />
        <MetricCard title="Pending Reviews" value={status?.pendingReviews ?? 0} />
        <MetricCard title="Active Repairs" value={status?.activeRepairs ?? 0} />
        <MetricCard title="Blocked" value={status?.blockedItems ?? 0} />
        <MetricCard title="Merge Ready" value={status?.mergeReadyItems ?? 0} />
      </div>

      <div className="grid gap-4 lg:grid-cols-[1fr_360px]">
        <Card>
          <CardHeader>
            <CardTitle>PR Queue</CardTitle>
          </CardHeader>
          <CardContent>
            {(items.data?.items ?? []).length === 0 ? (
              <div className="py-10 text-center text-sm text-muted-foreground">No pull requests recorded yet.</div>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>PR</TableHead>
                    <TableHead>Title</TableHead>
                    <TableHead>Head</TableHead>
                    <TableHead>CI</TableHead>
                    <TableHead>Review</TableHead>
                    <TableHead>Repair</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(items.data?.items ?? []).map((item) => (
                    <TableRow key={item.itemKey}>
                      <TableCell>#{item.number ?? item.itemKey}</TableCell>
                      <TableCell className="max-w-[360px] truncate">{item.title || '-'}</TableCell>
                      <TableCell className="font-mono text-xs">{shortSHA(item.headSHA ?? item.sha)}</TableCell>
                      <TableCell><Badge variant="outline">{item.ciState || 'unknown'}</Badge></TableCell>
                      <TableCell><Badge variant="secondary">{item.lastVerdict || 'unseen'}</Badge></TableCell>
                      <TableCell><Badge variant="outline">{item.repairState || 'none'}</Badge></TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Recent Runs</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {(runs.data?.items ?? []).length === 0 ? (
              <div className="py-8 text-center text-sm text-muted-foreground">No runs recorded yet.</div>
            ) : (
              (runs.data?.items ?? []).slice(0, 8).map((run) => (
                <div key={run.id} className="rounded-md border px-3 py-2">
                  <div className="flex items-center justify-between gap-2">
                    <span className="font-mono text-xs">{run.id}</span>
                    <Badge variant={run.phase === 'succeeded' ? 'default' : 'secondary'}>{run.phase}</Badge>
                  </div>
                  <div className="mt-1 text-xs text-muted-foreground">
                    {run.trigger} · {formatTime(run.startedAt)}
                  </div>
                </div>
              ))
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  )
}

function MetricCard({ title, value }: { title: string; value: number }) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">{title}</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-bold">{value}</div>
      </CardContent>
    </Card>
  )
}
