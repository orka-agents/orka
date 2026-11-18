import { Link } from '@tanstack/react-router'
import { ArrowLeft, Play } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { PageHeader } from '@/components/layout/page-header'
import { AccessErrorView, NotFoundView } from '@/components/common/not-found-page'
import { ListAccessError } from '@/components/ui/list-access-error'
import { StatusProblemAlert } from '@/components/common/status-problem-alert'
import { statusProblem } from '@/components/common/status-problem'
import { useCreateRepositoryMonitorCommand, useRepositoryMonitor, useRepositoryMonitorActions, useRepositoryMonitorCommands, useRepositoryMonitorImplementationJobs, useRepositoryMonitorItems, useRepositoryMonitorMutations, useRepositoryMonitorRuns, useRepositoryMonitorWorkActions, useRunRepositoryMonitor } from '@/hooks/use-monitors'
import { repositoryMonitorDisplayName } from './repository-monitor-display'
import { isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { formatTimestamp } from '@/lib/time'
import { useUIStore } from '@/stores/ui'
import type { RepositoryMonitor } from '@/schemas/monitor'

function shortSHA(value?: string) {
  if (!value) return '-'
  return value.slice(0, 8)
}

function publishBadgeVariant(phase?: string): 'default' | 'destructive' | 'outline' | 'secondary' {
  if (phase === 'succeeded') return 'default'
  if (phase === 'failed') return 'destructive'
  if (phase === 'skipped') return 'outline'
  return 'secondary'
}

function publishLabel(phase?: string, reason?: string) {
  if (!phase) return 'not attempted'
  if (phase === 'succeeded') return 'posted'
  if (reason) return `${phase}: ${reason}`
  return phase
}

export function RepositoryMonitorDetail({ monitorName }: { monitorName: string }) {
  const namespace = useUIStore((s) => s.namespace)
  const { data: monitor, isLoading, error } = useRepositoryMonitor(monitorName)

  if (isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-10 w-80" />
        <Skeleton className="h-32 w-full" />
      </div>
    )
  }

  const backToMonitors = (
    <Button asChild variant="outline">
      <Link to="/monitors"><ArrowLeft className="mr-1 h-4 w-4" />Back to Monitors</Link>
    </Button>
  )

  // A 403 replaces cached data (the token lost access); any other failure
  // keeps what already loaded and only blocks a page that never loaded.
  if (isForbiddenError(error) || (error && !isNotFoundError(error) && !monitor)) {
    return <AccessErrorView title="Could not load repository monitor" resource={`repository monitor ${monitorName}`} error={error} action={backToMonitors} />
  }

  // A 404 wins over cached data: a monitor deleted after it loaded keeps its
  // last `data`. The detail queries live in RepositoryMonitorDetailContent, so
  // a missing monitor sends none of them.
  if (isNotFoundError(error) || !monitor) {
    return (
      <NotFoundView
        title="Repository monitor not found"
        headline={`No repository monitor named "${monitorName}" in ${namespace}`}
        hint="It may have been deleted, or it belongs to another namespace."
        action={backToMonitors}
      />
    )
  }

  return <RepositoryMonitorDetailContent monitorName={monitorName} monitor={monitor} />
}

function RepositoryMonitorDetailContent({ monitorName, monitor }: { monitorName: string; monitor: RepositoryMonitor }) {
  const runs = useRepositoryMonitorRuns(monitorName)
  const items = useRepositoryMonitorItems(monitorName)
  const issueItems = useRepositoryMonitorItems(monitorName, 'issue')
  const actions = useRepositoryMonitorActions(monitorName)
  const commands = useRepositoryMonitorCommands(monitorName)
  const workActions = useRepositoryMonitorWorkActions(monitorName)
  const implementationJobs = useRepositoryMonitorImplementationJobs(monitorName)
  const mutations = useRepositoryMonitorMutations(monitorName)
  const runMonitor = useRunRepositoryMonitor(monitorName)
  const createCommand = useCreateRepositoryMonitorCommand(monitorName)
  const sendCommand = (command: Parameters<typeof createCommand.mutate>[0]) =>
    createCommand.mutate(command, {
      onSuccess: () => toast.success(`Queued ${command.intent.replace(/_/g, ' ')} for #${command.number}`),
      onError: (error) => toast.error(`Failed to queue ${command.intent.replace(/_/g, ' ')}: ${error instanceof Error ? error.message : 'Unknown error'}`),
    })

  const status = monitor.status
  const displayName = repositoryMonitorDisplayName(monitor)
  const problem = statusProblem(status)

  return (
    <div className="space-y-4">
      <PageHeader
        title={monitor.metadata.name}
        description={displayName}
        action={
          <>
            <Badge variant={status?.phase === 'Ready' ? 'default' : 'secondary'}>{status?.phase || 'Pending'}</Badge>
            <Button
              variant="secondary"
              onClick={async () => {
                try {
                  await runMonitor.mutateAsync()
                  toast.success('Monitor run started')
                } catch (error) {
                  toast.error(`Failed to start monitor run: ${error instanceof Error ? error.message : 'Unknown error'}`)
                }
              }}
              disabled={runMonitor.isPending}
            >
              <Play className="mr-2 h-4 w-4" />
              Run
            </Button>
          </>
        }
      />

      {problem && <StatusProblemAlert problem={problem} />}

      <div className="grid gap-4 md:grid-cols-6">
        <MetricCard title="Open PRs" value={status?.openPullRequests ?? 0} />
        <MetricCard title="Open Issues" value={status?.openIssues ?? 0} />
        <MetricCard title="Pending Reviews" value={status?.pendingReviews ?? 0} />
        <MetricCard title="Pending Issue Actions" value={status?.pendingIssueActions ?? 0} />
        <MetricCard title="Blocked" value={(status?.blockedItems ?? 0) + (status?.blockedIssues ?? 0)} />
        <MetricCard title="Merge Ready" value={status?.mergeReadyItems ?? 0} />
      </div>

      <div className="grid gap-4 lg:grid-cols-[1fr_360px]">
        <div className="min-w-0 space-y-4">
        <Card>
          <CardHeader>
            <CardTitle>PR Queue</CardTitle>
          </CardHeader>
          <CardContent>
            {items.error && (isForbiddenError(items.error) || !items.data) ? (
              <ListAccessError error={items.error} resource="pull requests" />
            ) : (items.data?.items ?? []).length === 0 ? (
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
                    <TableHead>Publish</TableHead>
                    <TableHead>Repair</TableHead>
                    <TableHead>Commands</TableHead>
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
                      <TableCell>
                        {item.lastPublishURL ? (
                          <a href={item.lastPublishURL} target="_blank" rel="noreferrer">
                            <Badge variant={publishBadgeVariant(item.lastPublishPhase)}>{publishLabel(item.lastPublishPhase, item.lastPublishReason)}</Badge>
                          </a>
                        ) : (
                          <Badge variant={publishBadgeVariant(item.lastPublishPhase)}>{publishLabel(item.lastPublishPhase, item.lastPublishReason)}</Badge>
                        )}
                      </TableCell>
                      <TableCell><Badge variant="outline">{item.repairState || 'none'}</Badge></TableCell>
                      <TableCell className="space-x-1">
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'pull_request', number: item.number ?? 0, intent: 'review', targetSHA: item.headSHA ?? '' })} disabled={createCommand.isPending || !item.number || !item.headSHA}>Review</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'pull_request', number: item.number ?? 0, intent: 'fix', targetSHA: item.headSHA ?? '' })} disabled={createCommand.isPending || !item.number || !item.headSHA}>Fix</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'pull_request', number: item.number ?? 0, intent: 'fix_ci', targetSHA: item.headSHA ?? '' })} disabled={createCommand.isPending || !item.number || !item.headSHA}>Fix CI</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'pull_request', number: item.number ?? 0, intent: 'update_branch', targetSHA: item.headSHA ?? '' })} disabled={createCommand.isPending || !item.number || !item.headSHA}>Update branch</Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Issue Inventory</CardTitle>
          </CardHeader>
          <CardContent>
            {issueItems.error && (isForbiddenError(issueItems.error) || !issueItems.data) ? (
              <ListAccessError error={issueItems.error} resource="issues" />
            ) : (issueItems.data?.items ?? []).length === 0 ? (
              <div className="py-10 text-center text-sm text-muted-foreground">No issues recorded yet.</div>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Issue</TableHead>
                    <TableHead>Title</TableHead>
                    <TableHead>Phase</TableHead>
                    <TableHead>Command</TableHead>
                    <TableHead>Skip reason</TableHead>
                    <TableHead>Commands</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(issueItems.data?.items ?? []).map((item) => (
                    <TableRow key={item.itemKey}>
                      <TableCell>#{item.number ?? item.itemKey}</TableCell>
                      <TableCell className="max-w-[360px] truncate">{item.title || '-'}</TableCell>
                      <TableCell><Badge variant="secondary">{item.workflowPhase || 'discovered'}</Badge></TableCell>
                      <TableCell><Badge variant="outline">{item.lastActionKind || item.lastCommandIntent || 'none'}</Badge></TableCell>
                      <TableCell>{item.skipReason || '-'}</TableCell>
                      <TableCell className="space-x-1">
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'issue', number: item.number ?? 0, intent: 'triage' })} disabled={createCommand.isPending || !item.number}>Triage</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'issue', number: item.number ?? 0, intent: 'research' })} disabled={createCommand.isPending || !item.number}>Research</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'issue', number: item.number ?? 0, intent: 'plan' })} disabled={createCommand.isPending || !item.number}>Plan</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'issue', number: item.number ?? 0, intent: 'implement' })} disabled={createCommand.isPending || !item.number}>Implement</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'issue', number: item.number ?? 0, intent: 'decompose' })} disabled={createCommand.isPending || !item.number}>Decompose</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'issue', number: item.number ?? 0, intent: 'stop' })} disabled={createCommand.isPending || !item.number}>Stop</Button>
                        <Button size="sm" variant="outline" onClick={() => sendCommand({ kind: 'issue', number: item.number ?? 0, intent: 'resume' })} disabled={createCommand.isPending || !item.number}>Resume</Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>

        </div>
        <div className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle>GitHub Publishing</CardTitle>
            </CardHeader>
            <CardContent className="space-y-2 text-sm">
              <div className="flex items-center justify-between gap-2">
                <span className="text-muted-foreground">Status</span>
                <Badge variant={monitor.spec.review?.publish?.enabled ? 'default' : 'outline'}>
                  {monitor.spec.review?.publish?.enabled ? 'enabled' : 'disabled'}
                </Badge>
              </div>
              <div className="flex items-center justify-between gap-2">
                <span className="text-muted-foreground">Mode</span>
                <span>{monitor.spec.review?.publish?.mode || 'summary_only'}</span>
              </div>
              <div className="flex items-center justify-between gap-2">
                <span className="text-muted-foreground">Event</span>
                <span>{monitor.spec.review?.publish?.event || 'COMMENT'}</span>
              </div>
              <p className="text-xs text-muted-foreground">V1 publishes neutral COMMENT reviews only. APPROVE and REQUEST_CHANGES are not exposed.</p>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Actions</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {actions.error && (isForbiddenError(actions.error) || !actions.data) ? (
              <ListAccessError error={actions.error} resource="monitor actions" />
            ) : (actions.data?.items ?? []).length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">No actions recorded yet.</div>
              ) : (
                (actions.data?.items ?? []).slice(0, 8).map((action) => (
                  <div key={action.id} className="rounded-md border px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-xs">{action.id}</span>
                      <Badge variant={action.verdict === 'failed' ? 'destructive' : 'secondary'}>{action.actionKind}</Badge>
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {action.kind} #{action.number} · {action.verdict || 'recorded'} · {formatTimestamp(action.createdAt)}
                    </div>
                    {action.summary ? <div className="mt-1 text-xs">{action.summary}</div> : null}
                  </div>
                ))
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Commands</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {commands.error && (isForbiddenError(commands.error) || !commands.data) ? (
              <ListAccessError error={commands.error} resource="monitor commands" />
            ) : (commands.data?.items ?? []).length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">No commands recorded yet.</div>
              ) : (
                (commands.data?.items ?? []).slice(0, 8).map((command) => (
                  <div key={command.id} className="rounded-md border px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-xs">{command.id}</span>
                      <Badge variant={command.status === 'accepted' ? 'default' : command.status === 'rejected' ? 'destructive' : 'secondary'}>{command.status || 'unknown'}</Badge>
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {command.kind} #{command.number} · {command.intent || command.label} · {formatTimestamp(command.createdAt)}
                    </div>
                    {command.error ? <div className="mt-1 text-xs text-destructive">{command.error}</div> : null}
                  </div>
                ))
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Workflow Timeline</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {workActions.error && (isForbiddenError(workActions.error) || !workActions.data) ? (
              <ListAccessError error={workActions.error} resource="work actions" />
            ) : (workActions.data?.items ?? []).length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">No workflow actions queued yet.</div>
              ) : (
                (workActions.data?.items ?? []).slice(0, 10).map((action) => (
                  <div key={action.id} className="rounded-md border px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-xs">{action.id}</span>
                      <Badge variant={action.status === 'blocked' || action.status === 'failed' || action.status === 'cancelled' ? 'destructive' : action.status === 'succeeded' ? 'default' : 'secondary'}>{action.status}</Badge>
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {action.targetKind} #{action.targetNumber} · {action.desiredAction || action.intent} · {action.phase || 'queued'} · {formatTimestamp(action.updatedAt)}
                    </div>
                    {action.taskName ? <div className="mt-1 font-mono text-xs">Task: {action.taskName}</div> : null}
                    {action.blockedReason || action.error ? (
                      <div className="mt-2 rounded bg-destructive/10 px-2 py-1 text-xs text-destructive">Why blocked: {action.blockedReason || action.error}</div>
                    ) : null}
                  </div>
                ))
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Implementation Jobs</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {implementationJobs.error && (isForbiddenError(implementationJobs.error) || !implementationJobs.data) ? (
              <ListAccessError error={implementationJobs.error} resource="implementation jobs" />
            ) : (implementationJobs.data?.items ?? []).length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">No implementation jobs yet.</div>
              ) : (
                (implementationJobs.data?.items ?? []).slice(0, 8).map((job) => (
                  <div key={job.id} className="rounded-md border px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-xs">{job.id}</span>
                      <Badge variant={job.phase === 'blocked' || job.error ? 'destructive' : job.phase === 'pr_opened' ? 'default' : 'secondary'}>{job.phase || 'unknown'}</Badge>
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">Issue #{job.issueNumber} · attempt {job.attempt ?? 0} · validation {job.validationState || 'pending'}</div>
                    {job.branch ? <div className="mt-1 font-mono text-xs">Branch: {job.branch}</div> : null}
                    {job.patchArtifactID ? <div className="mt-1 font-mono text-xs">Patch: {job.patchArtifactID}</div> : null}
                    {job.prNumber ? <div className="mt-1 text-xs">Linked PR #{job.prNumber}</div> : null}
                    {job.error ? <div className="mt-1 text-xs text-destructive">{job.error}</div> : null}
                  </div>
                ))
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>GitHub Mutations</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {mutations.error && (isForbiddenError(mutations.error) || !mutations.data) ? (
              <ListAccessError error={mutations.error} resource="mutations" />
            ) : (mutations.data?.items ?? []).length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">No GitHub mutations recorded yet.</div>
              ) : (
                (mutations.data?.items ?? []).slice(0, 8).map((mutation) => (
                  <div key={mutation.id} className="rounded-md border px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-xs">{mutation.operation}</span>
                      <Badge variant={mutation.status === 'failed' ? 'destructive' : mutation.status === 'succeeded' ? 'default' : 'secondary'}>{mutation.status || 'recorded'}</Badge>
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">{mutation.targetKind} #{mutation.targetNumber} · {mutation.reason || 'mutation'} · {formatTimestamp(mutation.createdAt)}</div>
                    {mutation.githubURL ? <div className="mt-1 truncate text-xs">{mutation.githubURL}</div> : null}
                    {mutation.error ? <div className="mt-1 text-xs text-destructive">{mutation.error}</div> : null}
                  </div>
                ))
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Recent Runs</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {/* A failed load must not read as "No runs"; a later failed poll keeps what loaded. */}
              {runs.error && (isForbiddenError(runs.error) || !runs.data) ? (
                <ListAccessError error={runs.error} resource="monitor runs" />
              ) : runs.isLoading ? (
                <Skeleton className="h-24 w-full" />
              ) : (runs.data?.items ?? []).length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">No runs recorded yet.</div>
              ) : (
                (runs.data?.items ?? []).slice(0, 8).map((run) => (
                  <div key={run.id} className="rounded-md border px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-xs">{run.id}</span>
                      <Badge variant={run.phase === 'succeeded' ? 'default' : 'secondary'}>{run.phase}</Badge>
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {run.trigger} · {formatTimestamp(run.startedAt)}
                    </div>
                  </div>
                ))
              )}
            </CardContent>
          </Card>
        </div>
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
