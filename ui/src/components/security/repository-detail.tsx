import { Link } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { AccessErrorView, NotFoundView } from '@/components/common/not-found-page'
import { ListAccessError } from '@/components/ui/list-access-error'
import { StatusProblemAlert } from '@/components/common/status-problem-alert'
import { statusProblem } from '@/components/common/status-problem'
import { useAllFindings, useDroppedFindings, useRepositoryScan, useReviewSlices, useRunSecurityScan, useScanRuns } from '@/hooks/use-security'
import { ThreatModelEditor } from './threat-model-editor'
import { RecommendedFindings } from './recommended-findings'
import { FindingTable } from './finding-table'
import { FindingCountBadges } from './finding-count-badges'
import { isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { repositoryDisplayName } from '@/lib/repository-name'
import { timeAgo } from '@/lib/time'
import { useUIStore } from '@/stores/ui'
import type { RepositoryScan } from '@/schemas/security'

export function RepositoryDetail({ repositoryName }: { repositoryName: string }) {
  const namespace = useUIStore((s) => s.namespace)
  const { data: repo, isLoading, error } = useRepositoryScan(repositoryName)

  if (isLoading) {
    return <Skeleton className="h-96 w-full" />
  }

  const backToSecurity = (
    <Button asChild variant="outline">
      <Link to="/security"><ArrowLeft className="mr-1 h-4 w-4" />Back to Security</Link>
    </Button>
  )

  // A 403 replaces cached data (the token lost access); any other failure
  // keeps what already loaded and only blocks a page that never loaded.
  if (isForbiddenError(error) || (error && !isNotFoundError(error) && !repo)) {
    return <AccessErrorView title="Could not load repository" resource={`repository ${repositoryName}`} error={error} action={backToSecurity} />
  }

  // A 404 wins over cached data: a repository deleted after it loaded keeps
  // its last `data`. The detail queries live in RepositoryDetailContent, so a
  // missing repository sends none of them.
  if (isNotFoundError(error) || !repo) {
    return (
      <NotFoundView
        title="Repository not found"
        headline={`No repository named "${repositoryName}" in ${namespace}`}
        hint="It may have been deleted, or it belongs to another namespace."
        action={backToSecurity}
      />
    )
  }

  return <RepositoryDetailContent repositoryName={repositoryName} repo={repo} />
}

function RepositoryDetailContent({ repositoryName, repo }: { repositoryName: string; repo: RepositoryScan }) {
  const findings = useAllFindings(repositoryName)
  const scanRuns = useScanRuns(repositoryName)
  const reviewSlices = useReviewSlices(repositoryName)
  const droppedFindings = useDroppedFindings(repositoryName, repo.status?.lastScanID)
  const runScan = useRunSecurityScan(repositoryName)
  const latestDropped = droppedFindings.data?.items ?? []
  const droppedByLayer = latestDropped.reduce<Record<string, number>>((acc, item) => {
    const layer = item.layer || 'unknown'
    acc[layer] = (acc[layer] ?? 0) + 1
    return acc
  }, {})
  const topDroppedReasons = Object.entries(latestDropped.reduce<Record<string, number>>((acc, item) => {
    acc[item.reason] = (acc[item.reason] ?? 0) + 1
    return acc
  }, {})).sort((a, b) => b[1] - a[1]).slice(0, 3)

  const problem = statusProblem(repo.status)

  return (
    <div className="space-y-4">
      <PageHeader
        title={repo.metadata.name}
        description={repositoryDisplayName(repo.spec, repo.spec.repoURL)}
        action={
          <>
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
          </>
        }
      />

      {problem && <StatusProblemAlert problem={problem} />}

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
              <FindingCountBadges counts={repo.status?.findingCounts} />
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Last Successful Scan</CardTitle></CardHeader>
          <CardContent>{timeAgo(repo.status?.lastSuccessfulScanAt, { empty: 'Never' })}</CardContent>
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
            {/* A failed request is not zero; show a dash instead of a count. */}
            <div className="text-2xl font-bold">{reviewSlices.error && !reviewSlices.data ? '—' : reviewSlices.data?.items?.length ?? 0}</div>
            <div className="mt-1 text-xs text-muted-foreground">Deterministic repository review units</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Accepted Output</CardTitle></CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{scanRuns.error && !scanRuns.data ? '—' : scanRuns.data?.items?.[0]?.acceptedFindings ?? 0}</div>
            <div className="mt-1 text-xs text-muted-foreground">Latest scan v2 findings accepted</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="text-base">Dropped Output</CardTitle></CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{droppedFindings.error && !droppedFindings.data && !scanRuns.data ? '—' : droppedFindings.data?.items?.length ?? scanRuns.data?.items?.[0]?.droppedFindings ?? 0}</div>
            <div className="mt-1 text-xs text-muted-foreground">Rejected model findings with diagnostics</div>
            <div className="mt-2 flex flex-wrap gap-1 text-xs">
              {Object.entries(droppedByLayer).map(([layer, count]) => (
                <Badge key={layer} variant="outline">{count} {layer}</Badge>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>

      {latestDropped.length > 0 && (
        <Card>
          <CardHeader><CardTitle>Scan Quality Diagnostics</CardTitle></CardHeader>
          <CardContent className="space-y-3 text-sm">
            <div className="text-muted-foreground">Latest scan dropped findings by validation, filter, or cap layer. Samples are sanitized server-side.</div>
            <div className="grid gap-2 md:grid-cols-3">
              {latestDropped.slice(0, 6).map((item) => (
                <div key={item.id} className="rounded-md border border-border p-2">
                  <div className="flex items-center justify-between gap-2">
                    <Badge variant="secondary">{item.layer || 'unknown'}</Badge>
                    <span className="text-xs text-muted-foreground">{timeAgo(item.createdAt, { empty: 'Never' })}</span>
                  </div>
                  <div className="mt-2 text-xs text-muted-foreground">{item.reason}</div>
                </div>
              ))}
            </div>
            {topDroppedReasons.length > 0 && (
              <div className="flex flex-wrap gap-2 text-xs">
                {topDroppedReasons.map(([reason, count]) => (
                  <Badge key={reason} variant="outline">{count}× {reason}</Badge>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      <ThreatModelEditor repositoryName={repositoryName} />
      <RecommendedFindings repositoryName={repositoryName} />

      <Card>
        <CardHeader>
          <CardTitle>All Findings</CardTitle>
        </CardHeader>
        <CardContent>
          {/* A failed load must not read as "No findings"; a later failed poll keeps what loaded. */}
          {findings.error && (isForbiddenError(findings.error) || !findings.data) ? (
            <ListAccessError error={findings.error} resource="findings" />
          ) : findings.isLoading ? (
            <Skeleton className="h-32 w-full" />
          ) : (
            <FindingTable findings={findings.data?.items ?? []} />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Recent Scan Runs</CardTitle>
        </CardHeader>
        <CardContent>
          {scanRuns.error && (isForbiddenError(scanRuns.error) || !scanRuns.data) ? (
            <ListAccessError error={scanRuns.error} resource="scan runs" />
          ) : scanRuns.isLoading ? (
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
                    Started {timeAgo(run.startedAt, { empty: 'Never' })} · Commits {run.commitCount ?? 0} · Slices {run.sliceCount ?? 0} · Accepted {run.acceptedFindings ?? 0} · Dropped {run.droppedFindings ?? 0} · Policy {run.scannerPolicyVersion || 'default'}
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
