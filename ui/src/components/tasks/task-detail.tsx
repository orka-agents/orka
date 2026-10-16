import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { toast } from 'sonner'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { AlertTriangle, ArrowLeft, SearchX, ShieldCheck, Trash2 } from 'lucide-react'
import { EmptyState } from '@/components/ui/empty-state'
import { PageHeader } from '@/components/layout/page-header'
import { TaskStatusBadge } from './task-status-badge'
import { PRStatusBadge } from './pr-status-badge'
import { TaskResultViewer } from './task-result-viewer'
import { StructuredLogViewer } from './structured-log-viewer'
import { TaskApprovalPanel } from './task-approval-panel'
import { ForkProvenance } from './fork-provenance'
import { RunTimeline } from './run-timeline'
import { TaskSchedulePanel } from './task-schedule-panel'
import { TaskOverview } from './task-overview'
import { TaskActivity } from './task-activity'
import { TaskArtifactsPanel } from './task-artifacts-panel'
import { TaskStatusDetails } from './task-status-details'
import { TaskYaml } from './task-yaml'
import { ViewSwitch } from './view-switch'
import { TaskExecutionRouteLedger } from '@/components/execution/execution-route-ledger'
import { useStickyQueryError } from '@/hooks/use-sticky-query-error'
import { useTask, useDeleteTask, useTaskEvents } from '@/hooks/use-tasks'
import { useTaskTrace, useTaskApprovals } from '@/hooks/use-execution-events'
import { useTaskArtifacts } from '@/hooks/use-task-artifacts'
import { ApiError, isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { useNavigate, useSearch } from '@tanstack/react-router'
import type { ExecutionEvent, PlanState } from '@/schemas/task'
import { timeAgo } from '@/lib/time'
import { formatShellCommand } from '@/lib/task-command'
import { actionErrorMessage } from './action-error'
import { isTaskDeleting, isWaitingForApproval } from './task-state'
import { TaskStateBadges } from './task-state-badges'

function latestEventPlan(events: ExecutionEvent[]): PlanState | undefined {
  let latest: ExecutionEvent | undefined
  for (const event of events) {
    if (event.type === 'PlanUpdated' && (!latest || event.seq > latest.seq)) {
      latest = event
    }
  }
  if (!latest) return undefined

  const content = latest.content
  const fields = content && typeof content === 'object' && !Array.isArray(content)
    ? content as Record<string, unknown>
    : undefined

  return {
    summary: latest.summary,
    progressPct: typeof fields?.progressPct === 'number' ? fields.progressPct : undefined,
    goalComplete: typeof fields?.goalComplete === 'boolean' ? fields.goalComplete : undefined,
    planDocument: latest.contentText,
  }
}

const SPEC_VIEWS = [
  { value: 'summary', label: 'Summary' },
  { value: 'yaml', label: 'YAML' },
] as const

// Tabs that were merged into others, so links and bookmarks to them still land.
const MERGED_TABS: Record<string, string> = {
  runtime: 'overview',
  execution: 'overview',
  children: 'overview',
  trace: 'activity',
  timeline: 'activity',
}

// Titled like the load-error pages: the header is the page's h1 and names
// the browser tab.
function TaskNotFound({ taskId }: { taskId: string }) {
  return (
    <div className="space-y-4">
      <PageHeader title="Task not found" />
      <Card>
        <CardContent>
          <EmptyState
            icon={SearchX}
            headline={`No task named "${taskId}" exists in this namespace.`}
            hint="It may have been deleted, or it belongs to another namespace."
            action={
              <Button asChild variant="outline" size="sm">
                <Link to="/tasks">
                  <ArrowLeft className="mr-1 h-4 w-4" /> Back to Tasks
                </Link>
              </Button>
            }
          />
        </CardContent>
      </Card>
    </div>
  )
}

// A forbidden task gets a titled page like the session, agent, and tool
// detail views: the header names the browser tab, and the card says why and
// links back to the list.
function TaskAccessError({ message, forbidden = true }: { message: string; forbidden?: boolean }) {
  return (
    <div className="space-y-4">
      <PageHeader title="Could not load task" />
      <Card>
        <CardContent>
          <div role="alert">
            <EmptyState
              icon={forbidden ? ShieldCheck : AlertTriangle}
              headline={forbidden ? 'Not authorized to view this task' : 'The task could not be loaded'}
              hint={forbidden
                ? `Your token lacks tasks read permission${message ? ` (${message})` : ''}.`
                : message || 'The task request failed. Try again in a moment.'}
              action={
                <Button asChild variant="outline" size="sm">
                  <Link to="/tasks">
                    <ArrowLeft className="mr-1 h-4 w-4" /> Back to Tasks
                  </Link>
                </Button>
              }
            />
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

export function TaskDetail({ taskId }: { taskId: string }) {
  const [following, setFollowing] = useState(true)
  const [specView, setSpecView] = useState<'summary' | 'yaml'>('summary')
  const { data: task, isLoading, error: queryTaskError, errorUpdatedAt } = useTask(taskId, following ? 5000 : false)
  // A failed first load keeps its error page while polling retries it.
  const taskError = useStickyQueryError({ data: task, error: queryTaskError, errorUpdatedAt })
  // Dependent queries (events, trace, approvals, artifacts) wait until the
  // task has loaded, and stay off once the task query 404s or 403s, so a
  // missing or forbidden task generates no hidden dependent traffic.
  const taskMissing = isNotFoundError(taskError)
  const taskForbidden = isForbiddenError(taskError)
  const taskUnavailable = taskMissing || taskForbidden || task === undefined
  const { data: taskEventsResponse, error: taskEventsError, failureReason: taskEventsFailureReason } = useTaskEvents(
    taskId,
    following ? 5000 : false,
    task?.metadata.uid,
    !taskUnavailable,
  )
  // Fork and the Overview's activity need execution-event storage; a 501 means it's off.
  // While retries are pending, failureReason carries the current fetch failure.
  const taskEventsIssue = taskEventsError ?? taskEventsFailureReason
  const taskEventsUnsupported = taskEventsIssue instanceof ApiError && taskEventsIssue.status === 501
  const taskEventsFailed = Boolean(taskEventsIssue) && !taskEventsUnsupported
  const taskEventsStatus = taskEventsUnsupported ? 'unsupported' : taskEventsFailed ? 'error' : undefined
  const forkSupported = !taskEventsUnsupported && !taskEventsFailed
  const taskEvents = taskEventsResponse?.events ?? []
  const plan = task?.plan ?? latestEventPlan(taskEvents)
  const hasPlanHistory = Boolean(plan) || (task?.status?.iteration ?? 0) > 0 ||
    taskEvents.some((event) => event.type === 'PlanUpdated')
  const deleteTask = useDeleteTask()
  const navigate = useNavigate()
  const search = useSearch({ from: '/tasks/$taskId' })
  // Local override gives instant tab response; it is cleared whenever the URL
  // search changes (deep link, back/forward, external link) so the URL stays the
  // source of truth and the override can't permanently shadow it.
  const [tabState, setTabState] = useState<{ override: string | null; seen?: string }>({ override: null })
  if (tabState.seen !== search.tab) setTabState({ override: null, seen: search.tab })
  const availableTabs = new Set(['overview', 'activity', 'approvals', 'result', 'logs', 'spec'])
  if (hasPlanHistory) availableTabs.add('plan')
  const rawTab = tabState.override ?? search.tab ?? 'overview'
  const requestedTab = MERGED_TABS[rawTab] ?? rawTab
  const activeTab = availableTabs.has(requestedTab) ? requestedTab : 'overview'
  const activityView = rawTab === 'timeline' ? 'events' : 'trace'
  const setTab = (tab: string) => {
    setTabState({ override: tab, seen: search.tab })
    navigate({ to: '/tasks/$taskId', params: { taskId }, search: { tab }, replace: true })
  }
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)
  // Scope the armed delete to the loaded task's namespace+uid+name; a namespace
  // switch or same-name/new-uid recreation drops a stale confirm so it can't
  // delete a different task than the one armed.
  const deleteIdentity = `${task?.metadata.namespace ?? ''}/${task?.metadata.uid ?? ''}/${taskId}`
  const deleteArmed = confirmDelete === deleteIdentity
  // isPending lands a render later; a double click on Confirm must send a
  // single DELETE.
  const deletingRef = useRef(false)
  // Arming swaps Delete for Confirm/Cancel and disarming swaps them back, so
  // the focused button unmounts either way: move focus to Confirm when armed,
  // and back to Delete after Cancel, Esc, or a failed delete unless focus has
  // moved elsewhere meanwhile.
  const deleteButtonRef = useRef<HTMLButtonElement>(null)
  const confirmDeleteButtonRef = useRef<HTMLButtonElement>(null)
  const wasDeleteArmedRef = useRef(false)
  useEffect(() => {
    if (deleteArmed) {
      confirmDeleteButtonRef.current?.focus()
    } else if (wasDeleteArmedRef.current && (!document.activeElement || document.activeElement === document.body)) {
      deleteButtonRef.current?.focus()
    }
    wasDeleteArmedRef.current = deleteArmed
  }, [deleteArmed])
  const confirmTaskDelete = async (name: string) => {
    if (deletingRef.current) return
    deletingRef.current = true
    try {
      await deleteTask.mutateAsync(name)
      toast.success(`Task ${name} deleted`)
      navigate({ to: '/tasks' })
    } catch (err) {
      // Disarm so the header is usable again; the toast carries the reason.
      setConfirmDelete(null)
      toast.error(actionErrorMessage('Failed to delete task', err))
    } finally {
      deletingRef.current = false
    }
  }
  // Overview data: fetched only when that tab is active so other tabs don't
  // pay for trace/approvals/artifacts. Each hook is namespace+uid scoped.
  const overviewActive = activeTab === 'overview' && !taskUnavailable
  const resultActive = activeTab === 'result' && !taskUnavailable
  const taskRunning = task?.status?.phase === 'Running'
  const taskTerminal = ['Succeeded', 'Failed', 'Cancelled'].includes(task?.status?.phase ?? '')
  const traceRefetchInterval = overviewActive && taskRunning && following ? 5000 : false
  const terminalTraceRefetchKey = useRef<string | null>(null)
  const terminalArtifactRefetchKey = useRef<string | null>(null)
  const { data: trace, refetch: refetchTrace } = useTaskTrace(
    taskId,
    overviewActive,
    task?.metadata.uid,
    traceRefetchInterval,
  )
  // Poll approvals while live and until execution recovery settles after termination.
  const approvalRefetchInterval = following ? 5000 : undefined
  // A task parked on an approval also loads approvals off the Overview so
  // the Approvals tab can show how many are pending.
  const waitingForApproval = task !== undefined && !taskUnavailable && isWaitingForApproval(task)
  const { data: approvalsResp } = useTaskApprovals(
    taskId,
    overviewActive || waitingForApproval,
    approvalRefetchInterval,
    taskRunning,
    taskTerminal,
    task?.metadata.uid,
  )
  // Terminal tasks reject decisions, so their leftovers are not "pending".
  const pendingApprovals = taskTerminal ? 0 : (approvalsResp?.approvals ?? []).filter((a) => a.status === 'pending').length
  const artifactRefetchInterval = overviewActive && taskRunning && following ? 5000 : false
  const resultArtifactRefetchInterval = resultActive && taskRunning && following ? 5000 : false
  const { data: artifactsResp, refetch: refetchArtifacts } = useTaskArtifacts(
    taskId,
    overviewActive,
    task?.metadata.uid,
    artifactRefetchInterval,
  )
  // Polling stops when the task finishes, so fetch once more for what it
  // published at the end. Artifacts show on both the Overview and Result.
  useEffect(() => {
    if (!taskTerminal || !task?.metadata.uid) return
    const key = `${task.metadata.uid}/${task.status?.phase ?? ''}`
    if (overviewActive && terminalTraceRefetchKey.current !== key) {
      terminalTraceRefetchKey.current = key
      refetchTrace()
    }
    if ((overviewActive || resultActive) && terminalArtifactRefetchKey.current !== key) {
      terminalArtifactRefetchKey.current = key
      refetchArtifacts()
    }
  }, [refetchArtifacts, refetchTrace, overviewActive, resultActive, task?.metadata.uid, task?.status?.phase, taskTerminal])

  // On narrow screens the tab bar scrolls sideways; keep the active tab in
  // view when it changes (or first renders) without scrolling the page. The
  // Approvals pending badge arrives with a later fetch and widens its tab and
  // shifts the ones after it, so a count change re-checks too.
  const tabsScrollRef = useRef<HTMLDivElement>(null)
  const tabsShown = Boolean(task) && !taskMissing && !taskForbidden
  useEffect(() => {
    const container = tabsScrollRef.current
    if (!container || container.scrollWidth <= container.clientWidth) return
    const active = container.querySelector<HTMLElement>('[role="tab"][data-state="active"]')
    if (!active) return
    const bounds = container.getBoundingClientRect()
    const tab = active.getBoundingClientRect()
    if (tab.left < bounds.left) container.scrollLeft -= bounds.left - tab.left
    else if (tab.right > bounds.right) container.scrollLeft += tab.right - bounds.right
  }, [activeTab, tabsShown, pendingApprovals])

  if (isLoading && !taskError) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  // A 403 is an actionable permission failure, not a missing task; it must
  // not fall through to "Task not found" just because `task` is undefined.
  if (taskForbidden) {
    return <TaskAccessError message={taskError.message} />
  }

  // Any other failure before the task ever loaded is an error, not a
  // missing task; a failed refetch keeps showing the task already loaded.
  if (taskError && !taskMissing && !task) {
    return <TaskAccessError forbidden={false} message={taskError instanceof Error ? taskError.message : ''} />
  }

  // A 404 wins over cached data: React Query keeps the last successful `data`
  // when a refetch fails, so a task deleted after it loaded would otherwise
  // keep rendering as if it still existed.
  if (taskMissing || !task) {
    return <TaskNotFound taskId={taskId} />
  }

  // The controller's parent annotation is authoritative; the label can be a
  // hashed selector value for long names.
  const parentTaskName = task.metadata.annotations?.['orka.ai/parent-task-name']?.trim() ||
    task.metadata.labels?.['orka.ai/parent-task']?.trim()
  // A fork is labeled as a child of its source; name the relation it has.
  const forkSourceName = task.metadata.annotations?.['orka.ai/fork-source-task']?.trim()
  const relatedTaskName = forkSourceName || parentTaskName

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex min-w-0 flex-wrap items-center gap-4">
          <Button asChild variant="ghost" size="icon">
            <Link to="/tasks" aria-label="Back to tasks">
              <ArrowLeft className="h-4 w-4" />
            </Link>
          </Button>
          <PageHeader
            title={task.metadata.name}
            description={`${task.metadata.namespace} · ${task.spec.type}`}
          />
          <TaskStatusBadge phase={task.status?.phase} />
          <TaskStateBadges task={task} />
          <PRStatusBadge annotations={task.metadata.annotations} />
          {relatedTaskName && (
            <p className="w-full text-sm text-muted-foreground">
              {forkSourceName ? 'Forked from' : 'Child of'}{' '}
              <Link to="/tasks/$taskId" params={{ taskId: relatedTaskName }} className="font-mono text-foreground hover:underline">
                {relatedTaskName}
              </Link>
            </p>
          )}
        </div>
        <div className="flex items-center gap-2">
          {isTaskDeleting(task) ? (
            // The API accepted the delete; finalizers are still running.
            <Button variant="destructive" size="sm" disabled title="Deletion in progress">
              <Trash2 className="mr-2 h-4 w-4" /> Deleting…
            </Button>
          ) : deleteArmed ? (
            <span
              className="flex items-center gap-1"
              onKeyDown={(e) => {
                if (e.key === 'Escape' && !deleteTask.isPending) {
                  e.preventDefault()
                  setConfirmDelete(null)
                }
              }}
            >
              <Button
                ref={confirmDeleteButtonRef}
                variant="destructive"
                size="sm"
                disabled={deleteTask.isPending}
                onClick={() => confirmTaskDelete(task.metadata.name)}
              >
                {deleteTask.isPending ? 'Deleting…' : 'Confirm delete'}
              </Button>
              <Button variant="ghost" size="sm" onClick={() => setConfirmDelete(null)} disabled={deleteTask.isPending}>
                Cancel
              </Button>
            </span>
          ) : (
            <Button
              ref={deleteButtonRef}
              variant="destructive"
              size="sm"
              onClick={() => setConfirmDelete(deleteIdentity)}
              title={['Succeeded', 'Failed', 'Cancelled'].includes(task.status?.phase ?? '') ? 'Delete task' : 'Delete (cancels a running task)'}
            >
              <Trash2 className="mr-2 h-4 w-4" /> Delete
            </Button>
          )}
        </div>
      </div>

      <TaskExecutionRouteLedger task={task} />
      <TaskSchedulePanel task={task} />

      <Tabs value={activeTab} onValueChange={setTab}>
        <div ref={tabsScrollRef} className="overflow-x-auto pb-1">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="activity">Activity</TabsTrigger>
          <TabsTrigger value="approvals">
            Approvals
            {pendingApprovals > 0 && (
              <>
                {' '}
                <span className="rounded-full bg-status-running-bg px-1.5 text-xs tabular-nums text-status-running">
                  <span aria-hidden="true">{pendingApprovals}</span>
                  <span className="sr-only">{pendingApprovals} pending</span>
                </span>
              </>
            )}
          </TabsTrigger>
          <TabsTrigger value="result">Result</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="spec">Spec</TabsTrigger>
          {hasPlanHistory && (
            <TabsTrigger value="plan">Plan</TabsTrigger>
          )}
        </TabsList>
        </div>

        <TabsContent value="overview">
          <TaskOverview
            task={task}
            events={taskEvents}
            trace={trace}
            approvals={approvalsResp?.approvals}
            artifacts={artifactsResp?.artifacts}
            eventsStatus={taskEventsStatus}
            following={following}
            onToggleFollow={() => setFollowing((f) => !f)}
            forkSupported={forkSupported}
            latestSeq={taskEventsResponse?.latestSeq}
            pendingApprovals={pendingApprovals}
            onReviewApprovals={() => setTab('approvals')}
          />
        </TabsContent>

        <TabsContent value="activity">
          {/* Keyed by the view, so a link from ?tab=trace to ?tab=timeline switches it. */}
          <TaskActivity
            key={activityView}
            taskId={taskId}
            taskUid={task.metadata.uid}
            taskNamespace={task.metadata.namespace}
            taskPhase={task.status?.phase}
            initialView={activityView}
          />
        </TabsContent>

        <TabsContent value="spec" className="space-y-4">
          <ViewSwitch label="Spec view" value={specView} options={SPEC_VIEWS} onChange={setSpecView} />
          {specView === 'yaml' ? <TaskYaml task={task} /> : (
          <>
          <ForkProvenance annotations={task.metadata.annotations} />
          <Card>
            <CardHeader>
              <CardTitle>Metadata</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-2 text-sm md:grid-cols-2">
              <div>
                <span className="text-muted-foreground">UID:</span>{' '}
                <span className="font-mono text-xs break-all">{task.metadata.uid}</span>
              </div>
              <div>
                <span className="text-muted-foreground">Created:</span>{' '}
                <span className="tabular-nums">
                  {timeAgo(task.metadata.creationTimestamp)}
                </span>
              </div>
              <div>
                <span className="text-muted-foreground">Priority:</span>{' '}
                {task.spec.priority ?? 500}
              </div>
              <div>
                <span className="text-muted-foreground">Attempts:</span>{' '}
                {task.status?.attempts ?? 0}
              </div>
              {task.status?.jobName && (
                <div>
                  <span className="text-muted-foreground">Job:</span>{' '}
                  <span className="font-mono text-xs break-all">
                    {task.status.jobName}
                  </span>
                </div>
              )}
              {task.status?.startTime && (
                <div>
                  <span className="text-muted-foreground">Started:</span>{' '}
                  <span className="tabular-nums">
                    {timeAgo(task.status.startTime)}
                  </span>
                </div>
              )}
              {task.status?.completionTime && (
                <div>
                  <span className="text-muted-foreground">Completed:</span>{' '}
                  <span className="tabular-nums">
                    {timeAgo(task.status.completionTime)}
                  </span>
                </div>
              )}
              {task.status?.message && (
                <div className="md:col-span-2 break-words">
                  <span className="text-muted-foreground">Message:</span>{' '}
                  {task.status.message}
                </div>
              )}
            </CardContent>
          </Card>

          {task.spec.type === 'container' && (
            <Card>
              <CardHeader>
                <CardTitle>Container Config</CardTitle>
              </CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div>
                  <span className="text-muted-foreground">Image:</span>{' '}
                  {task.spec.image}
                </div>
                {task.spec.command && (
                  <div>
                    <span className="text-muted-foreground">Command:</span>{' '}
                    {formatShellCommand(task.spec.command)}
                  </div>
                )}
                {task.spec.args && (
                  <div>
                    <span className="text-muted-foreground">Args:</span>{' '}
                    {formatShellCommand(task.spec.args)}
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {task.spec.type === 'ai' && (task.spec.ai || task.spec.agentRef) && (
            <Card>
              <CardHeader>
                <CardTitle>AI Config</CardTitle>
              </CardHeader>
              <CardContent className="space-y-2 text-sm">
                {task.spec.agentRef && (
                  <div>
                    <span className="text-muted-foreground">Agent:</span>{' '}
                    {!task.spec.agentRef.namespace || task.spec.agentRef.namespace === task.metadata.namespace ? (
                      <Link
                        to="/agents/$agentId"
                        params={{ agentId: task.spec.agentRef.name }}
                        className="underline-offset-4 hover:underline"
                      >
                        {task.spec.agentRef.name}
                      </Link>
                    ) : (
                      // The agent detail route resolves in the dashboard's
                      // current namespace, so a cross-namespace reference is
                      // shown qualified instead of linking to the wrong Agent.
                      <span className="font-mono">
                        {task.spec.agentRef.namespace}/{task.spec.agentRef.name}
                      </span>
                    )}
                  </div>
                )}
                <div>
                  <span className="text-muted-foreground">Provider:</span>{' '}
                  {task.spec.ai?.provider || (task.spec.agentRef ? <span className="text-muted-foreground">Agent default</span> : '-')}
                </div>
                <div>
                  <span className="text-muted-foreground">Model:</span>{' '}
                  {task.spec.ai?.model || (task.spec.agentRef ? <span className="text-muted-foreground">Agent default</span> : '-')}
                </div>
                {task.spec.ai?.prompt && (
                  <div>
                    <span className="text-muted-foreground">Prompt:</span>
                    <pre className="mt-1 overflow-x-auto rounded-md bg-muted p-3 whitespace-pre-wrap break-words">
                      {task.spec.ai.prompt}
                    </pre>
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {task.spec.type === 'agent' && task.spec.agentRef && (
            <Card>
              <CardHeader>
                <CardTitle>Agent Config</CardTitle>
              </CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div>
                  <span className="text-muted-foreground">Agent:</span>{' '}
                  {task.spec.agentRef.name}
                </div>
                {task.spec.prompt && (
                  <div>
                    <span className="text-muted-foreground">Prompt:</span>
                    <pre className="mt-1 overflow-x-auto rounded-md bg-muted p-3 whitespace-pre-wrap break-words">
                      {task.spec.prompt}
                    </pre>
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          <TaskStatusDetails task={task} />
          </>
          )}
        </TabsContent>

        <TabsContent value="result" className="space-y-4">
          <TaskResultViewer taskId={taskId} taskPhase={task.status?.phase} taskUid={task.metadata.uid} />
          <TaskArtifactsPanel taskId={taskId} taskUid={task.metadata.uid} refetchInterval={resultArtifactRefetchInterval} />
        </TabsContent>

        <TabsContent value="approvals">
          <TaskApprovalPanel taskId={taskId} taskPhase={task.status?.phase} taskUid={task.metadata.uid} />
        </TabsContent>

        <TabsContent value="logs">
          <StructuredLogViewer taskId={taskId} taskPhase={task.status?.phase} />
        </TabsContent>

        {hasPlanHistory && (
          <TabsContent value="plan">
            <Card>
              <CardHeader>
                <CardTitle>Agent Plan</CardTitle>
              </CardHeader>
              <CardContent>
                <div className="space-y-4">
                  <RunTimeline task={task} plan={plan} />
                  {plan?.planDocument && (
                    <div>
                      <p className="mb-1 text-xs font-medium text-muted-foreground">
                        Plan document
                      </p>
                      <pre className="rounded-md bg-muted p-4 whitespace-pre-wrap break-words max-h-[600px] overflow-auto text-xs">
                        {plan.planDocument}
                      </pre>
                    </div>
                  )}
                </div>
              </CardContent>
            </Card>
          </TabsContent>
        )}

      </Tabs>
    </div>
  )
}
