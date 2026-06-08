import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { useAllFindings, useDroppedFindings, useRepositoryScan, useReviewSlices, useRunSecurityScan, useScanRuns } from '@/hooks/use-security'
import { ThreatModelEditor } from './threat-model-editor'
import { RecommendedFindings } from './recommended-findings'
import { FindingTable } from './finding-table'

function timeAgo(ts?: string) {
  if (!ts) return 'Never'
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(ts).getTime()) / 1000))
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

export function RepositoryDetail({ repositoryName }: { repositoryName: string }) {
  const { data: repo, isLoading } = useRepositoryScan(repositoryName)
  const findings = useAllFindings(repositoryName)
  const scanRuns = useScanRuns(repositoryName)
  const reviewSlices = useReviewSlices(repositoryName)
  const droppedFindings = useDroppedFindings(repositoryName, repo?.status?.lastScanID)
  const runScan = useRunSecurityScan(repositoryName)

  if (isLoading) {
    return <Skeleton className="h-96 w-full" />
  }

  if (!repo) {
    return <div className="text-muted-foreground">Repository not found.</div>
  }

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">{repo.spec.owner}/{repo.spec.repository}</h1>
          <p className="text-muted-foreground">{repo.spec.repoURL}</p>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant={repo.status?.phase === 'Ready' ? 'default' : 'secondary'}>{repo.status?.phase || 'Pending'}</Badge>
          <Button
            onClick={async () => {
              try {
                await runScan.mutateAsync()
                toast.success('Manual scan started')
              } catch (error) {
                toast.error(`Failed to start scan: ${error instanceof Error ? error.message : 'Unknown error'}`)
              }
            }}
            disabled={runScan.isPending}
          >
            Scan Now
          </Button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-4">
        <Card>
          <CardHeader><CardTitle className="text-base">Branch</CardTitle></CardHeader>
          <CardContent>{repo.spec.branch || 'main'}</CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Open Findings</CardTitle></CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{repo.status?.findingCounts?.total ?? 0}</div>
            <div className="mt-2 flex flex-wrap gap-2 text-xs">
              <Badge variant="destructive">{repo.status?.findingCounts?.critical ?? 0} critical</Badge>
              <Badge variant="destructive">{repo.status?.findingCounts?.high ?? 0} high</Badge>
              <Badge variant="secondary">{repo.status?.findingCounts?.medium ?? 0} medium</Badge>
              <Badge variant="outline">{repo.status?.findingCounts?.low ?? 0} low</Badge>
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Last Successful Scan</CardTitle></CardHeader>
          <CardContent>{timeAgo(repo.status?.lastSuccessfulScanAt)}</CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Threat Model Version</CardTitle></CardHeader>
          <CardContent>{repo.status?.threatModelVersion ?? 0}</CardContent>
        </Card>
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader><CardTitle className="text-base">Review Slices</CardTitle></CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{reviewSlices.data?.items?.length ?? 0}</div>
            <div className="mt-1 text-xs text-muted-foreground">Deterministic repository review units</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Accepted Output</CardTitle></CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{scanRuns.data?.items?.[0]?.acceptedFindings ?? 0}</div>
            <div className="mt-1 text-xs text-muted-foreground">Latest scan v2 findings accepted</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Dropped Output</CardTitle></CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{droppedFindings.data?.items?.length ?? scanRuns.data?.items?.[0]?.droppedFindings ?? 0}</div>
            <div className="mt-1 text-xs text-muted-foreground">Rejected model findings with diagnostics</div>
          </CardContent>
        </Card>
      </div>

      <ThreatModelEditor repositoryName={repositoryName} />
      <RecommendedFindings repositoryName={repositoryName} />

      <Card>
        <CardHeader>
          <CardTitle>All Findings</CardTitle>
        </CardHeader>
        <CardContent>
          <FindingTable findings={findings.data?.items ?? []} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Recent Scan Runs</CardTitle>
        </CardHeader>
        <CardContent>
          {scanRuns.isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : (scanRuns.data?.items ?? []).length === 0 ? (
            <div className="text-sm text-muted-foreground">No scan runs yet.</div>
          ) : (
            <div className="space-y-3">
              {(scanRuns.data?.items ?? []).map((run) => (
                <div key={run.id} className="rounded-md border border-border p-3 text-sm">
                  <div className="flex items-center justify-between gap-3">
                    <div className="font-medium">{run.mode}</div>
                    <Badge variant={run.phase === 'succeeded' ? 'default' : 'secondary'}>{run.phase}</Badge>
                  </div>
                  <div className="mt-1 text-muted-foreground">{run.summary || run.taskName}</div>
                  <div className="mt-2 text-xs text-muted-foreground">
                    Started {timeAgo(run.startedAt)} · Commits {run.commitCount ?? 0} · Slices {run.sliceCount ?? 0} · Dropped {run.droppedFindings ?? 0}
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
