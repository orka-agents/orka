import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ArrowLeft, Trash2 } from 'lucide-react'
import { TaskStatusBadge } from './task-status-badge'
import { PRStatusBadge } from './pr-status-badge'
import { PRCreateDialog } from './pr-create-dialog'
import { TaskResultViewer } from './task-result-viewer'
import { StructuredLogViewer } from './structured-log-viewer'
import { TaskExecutionPanel } from './task-execution-panel'
import { TaskEventTimeline } from './task-event-timeline'
import { useTask, useDeleteTask } from '@/hooks/use-tasks'
import { useNavigate } from '@tanstack/react-router'

function timeAgo(ts?: string): string {
  if (!ts) return '-'
  const s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export function TaskDetail({ taskId }: { taskId: string }) {
  const { data: task, isLoading } = useTask(taskId)
  const deleteTask = useDeleteTask()
  const navigate = useNavigate()

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
          <Link to="/tasks"><Button variant="ghost" size="icon"><ArrowLeft className="h-4 w-4" /></Button></Link>
          <div>
            <h1 className="text-3xl font-bold tracking-tight">{task.metadata.name}</h1>
            <p className="text-muted-foreground">{task.metadata.namespace} · {task.spec.type}</p>
          </div>
          <TaskStatusBadge phase={task.status?.phase} />
          <PRStatusBadge annotations={task.metadata.annotations} />
        </div>
        <div className="flex items-center gap-2">
          {task.status?.phase === 'Succeeded' && task.spec.agentRuntime?.workspace?.pushBranch && (
            <PRCreateDialog
              taskName={task.metadata.name}
              pushBranch={task.spec.agentRuntime.workspace.pushBranch}
            />
          )}
          <Button
            variant="destructive"
            size="sm"
            onClick={async () => {
              await deleteTask.mutateAsync(task.metadata.name)
              navigate({ to: '/tasks' })
            }}
          >
            <Trash2 className="mr-2 h-4 w-4" /> Delete
          </Button>
        </div>
      </div>

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="execution">Execution</TabsTrigger>
          <TabsTrigger value="timeline">Timeline</TabsTrigger>
          <TabsTrigger value="result">Result</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          {(task.status?.iteration ?? 0) > 0 && (
            <TabsTrigger value="plan">Plan</TabsTrigger>
          )}
          {(task.status?.childTasks?.length ?? 0) > 0 && (
            <TabsTrigger value="children">Children</TabsTrigger>
          )}
        </TabsList>

        <TabsContent value="overview" className="space-y-4">
          <Card>
            <CardHeader><CardTitle>Metadata</CardTitle></CardHeader>
            <CardContent className="grid gap-2 text-sm md:grid-cols-2">
              <div><span className="text-muted-foreground">UID:</span> {task.metadata.uid}</div>
              <div><span className="text-muted-foreground">Created:</span> {timeAgo(task.metadata.creationTimestamp)}</div>
              <div><span className="text-muted-foreground">Priority:</span> {task.spec.priority ?? 500}</div>
              <div><span className="text-muted-foreground">Attempts:</span> {task.status?.attempts ?? 0}</div>
              {task.status?.jobName && <div><span className="text-muted-foreground">Job:</span> {task.status.jobName}</div>}
              {task.status?.startTime && <div><span className="text-muted-foreground">Started:</span> {timeAgo(task.status.startTime)}</div>}
              {task.status?.completionTime && <div><span className="text-muted-foreground">Completed:</span> {timeAgo(task.status.completionTime)}</div>}
              {task.status?.message && <div className="md:col-span-2"><span className="text-muted-foreground">Message:</span> {task.status.message}</div>}
              {(task.status?.iteration ?? 0) > 0 && <div><span className="text-muted-foreground">Iteration:</span> {task.status?.iteration}</div>}
            </CardContent>
          </Card>

          {(task.status?.iteration ?? 0) > 0 && !!(task as Record<string, unknown>).plan && (
            <Card>
              <CardHeader><CardTitle>Autonomous Plan</CardTitle></CardHeader>
              <CardContent className="space-y-3 text-sm">
                {(() => {
                  const plan = (task as Record<string, unknown>).plan as { summary?: string; progressPct?: number; goalComplete?: boolean; planDocument?: string }
                  return (
                    <>
                      {plan.summary && <div><span className="text-muted-foreground">Summary:</span> {plan.summary}</div>}
                      {plan.progressPct !== undefined && (
                        <div className="space-y-1">
                          <div className="flex justify-between">
                            <span className="text-muted-foreground">Progress:</span>
                            <span>{plan.progressPct}%</span>
                          </div>
                          <div className="h-2 rounded-full bg-muted overflow-hidden">
                            <div className="h-full rounded-full bg-primary transition-all" style={{ width: `${plan.progressPct}%` }} />
                          </div>
                        </div>
                      )}
                      {plan.goalComplete && <Badge variant="default">Goal Complete</Badge>}
                      {plan.planDocument && (
                        <div>
                          <span className="text-muted-foreground">Plan:</span>
                          <pre className="mt-1 rounded-md bg-muted p-3 whitespace-pre-wrap max-h-96 overflow-y-auto">{plan.planDocument}</pre>
                        </div>
                      )}
                    </>
                  )
                })()}
              </CardContent>
            </Card>
          )}

          {task.spec.type === 'container' && (
            <Card>
              <CardHeader><CardTitle>Container Config</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div><span className="text-muted-foreground">Image:</span> {task.spec.image}</div>
                {task.spec.command && <div><span className="text-muted-foreground">Command:</span> {task.spec.command.join(' ')}</div>}
                {task.spec.args && <div><span className="text-muted-foreground">Args:</span> {task.spec.args.join(' ')}</div>}
              </CardContent>
            </Card>
          )}

          {task.spec.type === 'ai' && task.spec.ai && (
            <Card>
              <CardHeader><CardTitle>AI Config</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div><span className="text-muted-foreground">Provider:</span> {task.spec.ai.provider}</div>
                <div><span className="text-muted-foreground">Model:</span> {task.spec.ai.model}</div>
                {task.spec.ai.prompt && (
                  <div>
                    <span className="text-muted-foreground">Prompt:</span>
                    <pre className="mt-1 rounded-md bg-muted p-3 whitespace-pre-wrap">{task.spec.ai.prompt}</pre>
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {task.spec.type === 'agent' && task.spec.agentRef && (
            <Card>
              <CardHeader><CardTitle>Agent Config</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div><span className="text-muted-foreground">Agent:</span> {task.spec.agentRef.name}</div>
                {task.spec.prompt && (
                  <div>
                    <span className="text-muted-foreground">Prompt:</span>
                    <pre className="mt-1 rounded-md bg-muted p-3 whitespace-pre-wrap">{task.spec.prompt}</pre>
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {(task.status?.conditions?.length ?? 0) > 0 && (
            <Card>
              <CardHeader><CardTitle>Conditions</CardTitle></CardHeader>
              <CardContent>
                <div className="space-y-2">
                  {task.status!.conditions!.map((c, i) => (
                    <div key={i} className="flex items-center gap-2 text-sm">
                      <Badge variant={c.status === 'True' ? 'default' : 'secondary'}>{c.type}</Badge>
                      <span className="text-muted-foreground">{c.status}</span>
                      {c.message && <span className="text-muted-foreground">— {c.message}</span>}
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
          <TaskExecutionPanel task={task} />
        </TabsContent>

        <TabsContent value="timeline">
          <TaskEventTimeline taskId={taskId} taskPhase={task.status?.phase} />
        </TabsContent>

        <TabsContent value="logs">
          <StructuredLogViewer taskId={taskId} taskPhase={task.status?.phase} />
        </TabsContent>

        {(task.status?.iteration ?? 0) > 0 && (
          <TabsContent value="plan">
            <Card>
              <CardHeader><CardTitle>Autonomous Plan</CardTitle></CardHeader>
              <CardContent>
                {(() => {
                  const plan = (task as Record<string, unknown>).plan as { summary?: string; progressPct?: number; goalComplete?: boolean; planDocument?: string } | undefined
                  if (!plan) return <p className="text-muted-foreground">No plan data available. Plan state is loaded when viewing task details.</p>
                  return (
                    <div className="space-y-3 text-sm">
                      {plan.summary && <div><span className="font-medium">Summary:</span> {plan.summary}</div>}
                      {plan.progressPct !== undefined && (
                        <div className="space-y-1">
                          <div className="flex justify-between">
                            <span className="font-medium">Progress:</span>
                            <span>{plan.progressPct}%</span>
                          </div>
                          <div className="h-2 rounded-full bg-muted overflow-hidden">
                            <div className="h-full rounded-full bg-primary transition-all" style={{ width: `${plan.progressPct}%` }} />
                          </div>
                        </div>
                      )}
                      {plan.goalComplete && <Badge variant="default">Goal Complete ✓</Badge>}
                      {plan.planDocument && (
                        <pre className="mt-2 rounded-md bg-muted p-4 whitespace-pre-wrap max-h-[600px] overflow-y-auto text-xs">{plan.planDocument}</pre>
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
              <CardHeader><CardTitle>Child Tasks</CardTitle></CardHeader>
              <CardContent>
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b text-left text-muted-foreground">
                        <th className="pb-2 pr-4">Name</th>
                        <th className="pb-2 pr-4">Agent</th>
                        <th className="pb-2">Phase</th>
                      </tr>
                    </thead>
                    <tbody>
                      {task.status!.childTasks!.map((child) => (
                        <tr key={child.name} className="border-b last:border-0">
                          <td className="py-2 pr-4">
                            <Link to="/tasks/$taskId" params={{ taskId: child.name }} className="text-blue-600 hover:underline dark:text-blue-400">
                              {child.name}
                            </Link>
                          </td>
                          <td className="py-2 pr-4">{child.agent}</td>
                          <td className="py-2"><TaskStatusBadge phase={child.phase} /></td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </CardContent>
            </Card>
          </TabsContent>
        )}
      </Tabs>
    </div>
  )
}
