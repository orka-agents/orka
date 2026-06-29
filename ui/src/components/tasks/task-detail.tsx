import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ArrowLeft, Trash2 } from 'lucide-react'
import { PageHeader } from '@/components/layout/page-header'
import { TaskStatusBadge } from './task-status-badge'
import { PRStatusBadge } from './pr-status-badge'
import { PRCreateDialog } from './pr-create-dialog'
import { TaskResultViewer } from './task-result-viewer'
import { StructuredLogViewer } from './structured-log-viewer'
import { TaskExecutionPanel } from './task-execution-panel'
import { TaskEventTimeline } from './task-event-timeline'
import { TaskTracePanel } from './task-trace-panel'
import { TaskApprovalPanel } from './task-approval-panel'
import { ForkProvenance } from './fork-provenance'
import { ExecutionGraph } from './execution-graph'
import { RunTimeline } from './run-timeline'
import { TaskRuntimeView } from '@/components/runtime/task-runtime-view'
import { useTask, useDeleteTask, useTaskEvents } from '@/hooks/use-tasks'
import { useTaskTrace, useTaskApprovals } from '@/hooks/use-execution-events'
import { useTaskArtifacts } from '@/hooks/use-task-artifacts'
import { ApiError } from '@/lib/api-client'
import { useNavigate, useSearch } from '@tanstack/react-router'

function timeAgo(ts?: string): string {
  if (!ts) return '-'
  const s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}


export function TaskDetail({ taskId }: { taskId: string }) {
  const [following, setFollowing] = useState(true)
  const { data: task, isLoading } = useTask(taskId)
  const { data: taskEventsResponse, error: taskEventsError } = useTaskEvents(
    taskId,
    following ? 5000 : false,
    task?.metadata.uid,
  )
  // Fork and the runtime timeline need execution-event storage; a 501 means it's off.
  const taskEventsUnsupported = taskEventsError instanceof ApiError && taskEventsError.status === 501
  const forkSupported = !taskEventsUnsupported
  const taskEvents = taskEventsResponse?.events ?? []
  const deleteTask = useDeleteTask()
  const navigate = useNavigate()
  const search = useSearch({ from: '/tasks/$taskId' })
  // Local override gives instant tab response; it is cleared whenever the URL
  // search changes (deep link, back/forward, external link) so the URL stays the
  // source of truth and the override can't permanently shadow it.
  const [tabState, setTabState] = useState<{ override: string | null; seen?: string }>({ override: null })
  if (tabState.seen !== search.tab) setTabState({ override: null, seen: search.tab })
  const availableTabs = new Set(['runtime', 'overview', 'execution', 'timeline', 'trace', 'approvals', 'result', 'logs'])
  if ((task?.status?.iteration ?? 0) > 0) availableTabs.add('plan')
  if ((task?.status?.childTasks?.length ?? 0) > 0) availableTabs.add('children')
  const requestedTab = tabState.override ?? search.tab ?? 'runtime'
  const activeTab = availableTabs.has(requestedTab) ? requestedTab : 'runtime'
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
  // Runtime-tab data: fetched only when that tab is active so other tabs don't
  // pay for trace/approvals/artifacts. Each hook is namespace+uid scoped.
  const runtimeActive = activeTab === 'runtime'
  const taskRunning = task?.status?.phase === 'Running'
  const taskTerminal = ['Succeeded', 'Failed', 'Cancelled'].includes(task?.status?.phase ?? '')
  const { data: trace } = useTaskTrace(taskId, runtimeActive, task?.metadata.uid)
  // Poll approvals while live so a new blocking approval surfaces in the runtime
  // health panel; stops once terminal (matches TaskApprovalPanel semantics).
  const { data: approvalsResp } = useTaskApprovals(taskId, runtimeActive, 5000, taskRunning, taskTerminal, task?.metadata.uid)
  const { data: artifactsResp } = useTaskArtifacts(taskId, runtimeActive, task?.metadata.uid)

  if (isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  if (!task) {
    return <div className="text-muted-foreground">Task not found</div>
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-4">
          <Link to="/tasks">
            <Button variant="ghost" size="icon" aria-label="Back to tasks">
              <ArrowLeft className="h-4 w-4" />
            </Button>
          </Link>
          <PageHeader
            title={task.metadata.name}
            description={`${task.metadata.namespace} · ${task.spec.type}`}
          />
          <TaskStatusBadge phase={task.status?.phase} />
          <PRStatusBadge annotations={task.metadata.annotations} />
        </div>
        <div className="flex items-center gap-2">
          {task.status?.phase === 'Succeeded' &&
            task.spec.agentRuntime?.workspace?.pushBranch && (
              <PRCreateDialog
                taskName={task.metadata.name}
                pushBranch={task.spec.agentRuntime.workspace.pushBranch}
              />
            )}
          {deleteArmed ? (
            <span className="flex items-center gap-1">
              <Button
                variant="destructive"
                size="sm"
                onClick={async () => {
                  await deleteTask.mutateAsync(task.metadata.name)
                  navigate({ to: '/tasks' })
                }}
              >
                Confirm delete
              </Button>
              <Button variant="ghost" size="sm" onClick={() => setConfirmDelete(null)}>
                Cancel
              </Button>
            </span>
          ) : (
            <Button variant="destructive" size="sm" onClick={() => setConfirmDelete(deleteIdentity)}>
              <Trash2 className="mr-2 h-4 w-4" /> Delete
            </Button>
          )}
        </div>
      </div>

      <Tabs value={activeTab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="runtime">Runtime</TabsTrigger>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="execution">Execution</TabsTrigger>
          <TabsTrigger value="timeline">Timeline</TabsTrigger>
          <TabsTrigger value="trace">Trace</TabsTrigger>
          <TabsTrigger value="approvals">Approvals</TabsTrigger>
          <TabsTrigger value="result">Result</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          {(task.status?.iteration ?? 0) > 0 && (
            <TabsTrigger value="plan">Plan</TabsTrigger>
          )}
          {(task.status?.childTasks?.length ?? 0) > 0 && (
            <TabsTrigger value="children">Children</TabsTrigger>
          )}
        </TabsList>

        <TabsContent value="runtime" className="space-y-4">
          <TaskRuntimeView
            task={task}
            events={taskEvents}
            trace={trace}
            approvals={approvalsResp?.approvals}
            artifacts={artifactsResp?.artifacts}
            following={following}
            onToggleFollow={() => setFollowing((f) => !f)}
            forkSupported={forkSupported}
            streamStatus={taskEventsUnsupported ? 'unsupported' : undefined}
            latestSeq={taskEventsResponse?.latestSeq}
          />
        </TabsContent>

        <TabsContent value="overview" className="space-y-4">
          <ForkProvenance annotations={task.metadata.annotations} />
          <Card>
            <CardHeader>
              <CardTitle>Metadata</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-2 text-sm md:grid-cols-2">
              <div>
                <span className="text-muted-foreground">UID:</span>{' '}
                <span className="font-mono text-xs">{task.metadata.uid}</span>
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
                  <span className="font-mono text-xs">
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
                <div className="md:col-span-2">
                  <span className="text-muted-foreground">Message:</span>{' '}
                  {task.status.message}
                </div>
              )}
            </CardContent>
          </Card>

          {(task.status?.iteration ?? 0) > 0 && (
            <Card>
              <CardHeader>
                <CardTitle>Autonomous Loop</CardTitle>
              </CardHeader>
              <CardContent>
                <RunTimeline
                  task={task}
                  plan={
                    (task as Record<string, unknown>).plan as
                      | import('@/schemas/task').PlanState
                      | undefined
                  }
                  events={taskEvents}
                />
              </CardContent>
            </Card>
          )}

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
                    {task.spec.command.join(' ')}
                  </div>
                )}
                {task.spec.args && (
                  <div>
                    <span className="text-muted-foreground">Args:</span>{' '}
                    {task.spec.args.join(' ')}
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {task.spec.type === 'ai' && task.spec.ai && (
            <Card>
              <CardHeader>
                <CardTitle>AI Config</CardTitle>
              </CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div>
                  <span className="text-muted-foreground">Provider:</span>{' '}
                  {task.spec.ai.provider}
                </div>
                <div>
                  <span className="text-muted-foreground">Model:</span>{' '}
                  {task.spec.ai.model}
                </div>
                {task.spec.ai.prompt && (
                  <div>
                    <span className="text-muted-foreground">Prompt:</span>
                    <pre className="mt-1 rounded-md bg-muted p-3 whitespace-pre-wrap">
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
                    <pre className="mt-1 rounded-md bg-muted p-3 whitespace-pre-wrap">
                      {task.spec.prompt}
                    </pre>
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {(task.status?.conditions?.length ?? 0) > 0 && (
            <Card>
              <CardHeader>
                <CardTitle>Conditions</CardTitle>
              </CardHeader>
              <CardContent>
                <div className="space-y-2">
                  {task.status!.conditions!.map((c, i) => (
                    <div key={i} className="flex items-center gap-2 text-sm">
                      <Badge
                        variant={c.status === 'True' ? 'default' : 'secondary'}
                      >
                        {c.type}
                      </Badge>
                      <span className="text-muted-foreground">{c.status}</span>
                      {c.message && (
                        <span className="text-muted-foreground">
                          — {c.message}
                        </span>
                      )}
                    </div>
                  ))}
                </div>
              </CardContent>
            </Card>
          )}
        </TabsContent>

        <TabsContent value="result">
          <TaskResultViewer taskId={taskId} />
        </TabsContent>

        <TabsContent value="execution">
          <TaskExecutionPanel task={task} events={taskEvents} />
        </TabsContent>

        <TabsContent value="timeline">
          <TaskEventTimeline
            key={`${task.metadata.namespace ?? ''}/${taskId}/${task.metadata.uid ?? ''}`}
            taskId={taskId}
            taskPhase={task.status?.phase}
            taskUid={task.metadata.uid}
          />
        </TabsContent>

        <TabsContent value="trace">
          <TaskTracePanel taskId={taskId} taskUid={task.metadata.uid} />
        </TabsContent>

        <TabsContent value="approvals">
          <TaskApprovalPanel taskId={taskId} taskPhase={task.status?.phase} taskUid={task.metadata.uid} />
        </TabsContent>

        <TabsContent value="logs">
          <StructuredLogViewer taskId={taskId} taskPhase={task.status?.phase} />
        </TabsContent>

        {(task.status?.iteration ?? 0) > 0 && (
          <TabsContent value="plan">
            <Card>
              <CardHeader>
                <CardTitle>Autonomous Plan</CardTitle>
              </CardHeader>
              <CardContent>
                {(() => {
                  const plan = (task as Record<string, unknown>).plan as
                    | import('@/schemas/task').PlanState
                    | undefined
                  return (
                    <div className="space-y-4">
                      <RunTimeline
                        task={task}
                        plan={plan}
                        events={taskEvents}
                      />
                      {plan?.planDocument && (
                        <div>
                          <p className="mb-1 text-xs font-medium text-muted-foreground">
                            Plan document
                          </p>
                          <pre className="rounded-md bg-muted p-4 whitespace-pre-wrap max-h-[600px] overflow-y-auto text-xs">
                            {plan.planDocument}
                          </pre>
                        </div>
                      )}
                    </div>
                  )
                })()}
              </CardContent>
            </Card>
          </TabsContent>
        )}

        {(task.status?.childTasks?.length ?? 0) > 0 && (
          <TabsContent value="children">
            <Card>
              <CardHeader>
                <CardTitle>Execution Graph</CardTitle>
              </CardHeader>
              <CardContent>
                <ExecutionGraph task={task} events={taskEvents} />
              </CardContent>
            </Card>
          </TabsContent>
        )}
      </Tabs>
    </div>
  )
}
