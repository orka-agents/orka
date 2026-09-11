import { useEffect, useState } from 'react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { TaskStatusBadge } from './task-status-badge'
import type { Task, ExecutionEvent } from '@/schemas/task'
import { latestModelContextEvent, latestModelUsageEvents } from '@/lib/execution-events'

const steps = ['Pending', 'Running', 'Completed'] as const

// Map a task phase onto the coarse Pending → Running → Completed stepper.
// Scheduled hasn't started yet, so it sits at the start (step 0). Running and
// Finalizing are both in progress. Only terminal phases
// (Succeeded/Failed/Cancelled) land on the final step.

function stepIndex(phase?: string): number {
  if (!phase || phase === 'Pending' || phase === 'Scheduled') return 0
  if (phase === 'Running' || phase === 'Finalizing') return 1
  return 2
}

function statusClass(value?: string) {
  switch (value) {
    case 'Succeeded':
    case 'VerifiedExact':
    case 'DeliveredSuperseded':
    case 'ReadValidated':
    case 'NoChange':
      return 'bg-status-succeeded-bg text-status-succeeded'
    case 'Running':
    case 'Accepted':
    case 'Settling':
    case 'Publishing':
    case 'Verifying':
    case 'Validating':
    case 'Preparing':
    case 'Prepared':
      return 'bg-status-running-bg text-status-running'
    case 'Failed':
    case 'OutcomeUnknown':
    case 'DeliveryConflict':
    case 'PublicationOutcomeUnknown':
    case 'ReadOnlyWorkspaceModified':
    case 'CredentialBlocked':
      return 'bg-status-failed-bg text-status-failed'
    default:
      return 'bg-muted text-muted-foreground'
  }
}

function StateBadge({ value }: { value?: string }) {
  return <Badge variant="secondary" className={statusClass(value)}>{value ?? 'Not reported'}</Badge>
}

function compact(value?: string) {
  if (!value) return '—'
  if (value.length <= 28) return value
  return `${value.slice(0, 16)}…${value.slice(-8)}`
}

function ElapsedTime({ startTime, completionTime }: { startTime?: string; completionTime?: string }) {
  const [elapsed, setElapsed] = useState(() => (startTime ? '' : '-'))

  useEffect(() => {
    if (!startTime) return
    const start = new Date(startTime).getTime()

    function update() {
      const end = completionTime ? new Date(completionTime).getTime() : Date.now()
      const seconds = Math.floor((end - start) / 1000)
      if (seconds < 60) setElapsed(`${seconds}s`)
      else if (seconds < 3600) setElapsed(`${Math.floor(seconds / 60)}m ${seconds % 60}s`)
      else setElapsed(`${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`)
    }

    update()
    if (completionTime) return
    const interval = setInterval(update, 1000)
    return () => clearInterval(interval)
  }, [startTime, completionTime])

  return <span data-testid="elapsed-time">{elapsed}</span>
}

function ChildTaskSummary({ childTasks }: { childTasks: NonNullable<Task['status']>['childTasks'] }) {
  if (!childTasks?.length) return null
  const running = childTasks.filter((child) => child.phase === 'Running').length
  const succeeded = childTasks.filter((child) => child.phase === 'Succeeded').length
  const failed = childTasks.filter((child) => child.phase === 'Failed').length
  const pending = childTasks.filter((child) => child.phase === 'Pending').length

  return (
    <div data-testid="child-task-summary">
      <span className="text-sm text-muted-foreground">Child Tasks: </span>
      <span className="text-sm">
        {childTasks.length} total
        {pending > 0 && <span className="text-yellow-600 dark:text-yellow-400"> · {pending} pending</span>}
        {running > 0 && <span className="text-blue-600 dark:text-blue-400"> · {running} running</span>}
        {succeeded > 0 && <span className="text-green-600 dark:text-green-400"> · {succeeded} succeeded</span>}
        {failed > 0 && <span className="text-red-600 dark:text-red-400"> · {failed} failed</span>}
      </span>
    </div>
  )
}

function DurableExecution({ task }: { task: Task }) {
  const execution = task.status?.execution
  if (!execution) return null
  const unknown = execution.state === 'OutcomeUnknown' || execution.outcome === 'OutcomeUnknown'

  return (
    <section className="space-y-3 rounded-md border p-4" aria-label="Durable execution status">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <p className="font-medium">Durable execution</p>
          <p className="text-xs text-muted-foreground">Prompt acceptance, settlement, and exact runtime fencing</p>
        </div>
        <div className="flex gap-2">
          <StateBadge value={execution.state} />
          {execution.outcome && <StateBadge value={execution.outcome} />}
        </div>
      </div>
      {unknown && (
        <div className="rounded-md border border-status-failed/30 bg-status-failed-bg p-3 text-sm text-status-failed">
          The runtime may have accepted this prompt, but Orka could not prove the outcome. It will not replay the prompt automatically.
        </div>
      )}
      <div className="grid gap-3 text-sm md:grid-cols-2 lg:grid-cols-3">
        <div><span className="text-muted-foreground">Attempt</span><div>{execution.attempt ?? task.status?.attempts ?? 0}</div></div>
        <div><span className="text-muted-foreground">Runtime pool</span><div>{execution.runtimePoolName ?? '—'}</div></div>
        <div><span className="text-muted-foreground">Session generation</span><div>{execution.runtimeSessionGeneration ?? '—'}</div></div>
        <div><span className="text-muted-foreground">Prompt ID</span><div className="font-mono text-xs" title={execution.promptID}>{compact(execution.promptID)}</div></div>
        <div><span className="text-muted-foreground">Runtime instance</span><div className="font-mono text-xs" title={execution.runtimeInstanceID}>{compact(execution.runtimeInstanceID)}</div></div>
        <div><span className="text-muted-foreground">Controller epoch</span><div>{execution.controllerEpoch ?? '—'}</div></div>
      </div>
      {(execution.reason || execution.message) && (
        <p className="text-sm text-muted-foreground">{[execution.reason, execution.message].filter(Boolean).join(' · ')}</p>
      )}
    </section>
  )
}

function WorkspaceDelivery({ task }: { task: Task }) {
  const delivery = task.status?.delivery
  const workspace = task.spec.workspace
  if (!delivery && !workspace) return null

  return (
    <section className="space-y-3 rounded-md border p-4" aria-label="Workspace delivery status">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <p className="font-medium">Workspace delivery</p>
          <p className="text-xs text-muted-foreground">Clean-room validation, publication, and independent remote verification</p>
        </div>
        <div className="flex gap-2">
          <Badge variant="outline" className="capitalize">{workspace?.intent ?? 'read'}</Badge>
          {delivery?.state && <StateBadge value={delivery.state} />}
          {delivery?.outcome && delivery.outcome !== delivery.state && <StateBadge value={delivery.outcome} />}
        </div>
      </div>
      <div className="grid gap-3 text-sm md:grid-cols-2 lg:grid-cols-3">
        <div><span className="text-muted-foreground">Source</span><div>{workspace?.gitRepo ?? delivery?.sourceRepository?.id ?? '—'}</div></div>
        <div><span className="text-muted-foreground">Publication branch</span><div>{delivery?.branch ?? workspace?.pushBranch ?? '—'}</div></div>
        <div><span className="text-muted-foreground">Publication ID</span><div className="font-mono text-xs" title={delivery?.publicationID}>{compact(delivery?.publicationID)}</div></div>
        <div><span className="text-muted-foreground">Expected commit</span><div className="font-mono text-xs" title={delivery?.expectedCommitSHA}>{compact(delivery?.expectedCommitSHA)}</div></div>
        <div><span className="text-muted-foreground">Verified remote</span><div className="font-mono text-xs" title={delivery?.verifiedRemoteSHA}>{compact(delivery?.verifiedRemoteSHA)}</div></div>
        <div><span className="text-muted-foreground">Artifact</span><div className="font-mono text-xs" title={delivery?.artifactDigest}>{compact(delivery?.artifactDigest)}</div></div>
      </div>
      {delivery?.prReceipt && (
        <div className="rounded-md bg-muted/50 p-3 text-sm">
          Pull request {delivery.prReceipt.number ? `#${delivery.prReceipt.number}` : delivery.prReceipt.id}
          {delivery.prReceipt.state ? ` · ${delivery.prReceipt.state}` : ''}
          {delivery.prReceipt.baseBranch && delivery.prReceipt.headBranch ? ` · ${delivery.prReceipt.headBranch} → ${delivery.prReceipt.baseBranch}` : ''}
        </div>
      )}
      {(delivery?.reason || delivery?.message) && (
        <p className="text-sm text-muted-foreground">{[delivery.reason, delivery.message].filter(Boolean).join(' · ')}</p>
      )}
    </section>
  )
}

export function TaskExecutionPanel({ task, events = [] }: { task: Task; events?: ExecutionEvent[] }) {
  const phase = task.status?.phase
  const modelEvents = latestModelUsageEvents(events)
  const contextEvent = latestModelContextEvent(events)
  const totalIn = modelEvents.reduce((sum, event) => sum + (event.inputTokens ?? 0), 0)
  const totalOut = modelEvents.reduce((sum, event) => sum + (event.outputTokens ?? 0), 0)
  const totalCachedIn = modelEvents.reduce((sum, event) => sum + (event.cachedInputTokens ?? 0), 0)
  const current = stepIndex(phase)

  return (
    <Card>
      <CardHeader>
        <CardTitle>Execution</CardTitle>
      </CardHeader>
      <CardContent className="space-y-6">
        <div className="flex items-center gap-2" data-testid="progress-steps">
          {steps.map((step, index) => (
            <div key={step} className="flex items-center gap-2">
              <div className={`flex h-8 w-8 items-center justify-center rounded-full text-xs font-medium ${
                index < current
                  ? 'bg-status-succeeded-bg text-status-succeeded'
                  : index === current
                    ? 'bg-status-running-bg text-status-running'
                    : 'bg-muted text-muted-foreground'
              }`}>
                {index < current ? '✓' : index + 1}
              </div>
              <span className={`text-sm ${index <= current ? 'font-medium' : 'text-muted-foreground'}`}>{step}</span>
              {index < steps.length - 1 && <div className={`h-0.5 w-8 ${index < current ? 'bg-status-succeeded' : 'bg-muted'}`} />}
            </div>
          ))}
        </div>

        <div className="grid gap-3 text-sm md:grid-cols-2">
          <div className="flex items-center gap-2"><span className="text-muted-foreground">Phase:</span><TaskStatusBadge phase={phase} /></div>
          <div className="flex items-center gap-2"><span className="text-muted-foreground">Elapsed:</span><ElapsedTime startTime={task.status?.startTime} completionTime={task.status?.completionTime} /></div>
          <div><span className="text-muted-foreground">Attempts:</span> {task.status?.attempts ?? 0}</div>
          {task.status?.message && <div className="md:col-span-2"><span className="text-muted-foreground">Message:</span> {task.status.message}</div>}
        </div>

        <DurableExecution task={task} />
        <WorkspaceDelivery task={task} />

        {modelEvents.length > 0 && (
          <div className="rounded-md border p-3 text-sm" aria-label="GenAI token rollup">
            <div className="font-medium">GenAI tokens</div>
            <div className="text-muted-foreground">
              {totalIn + totalOut} total · {totalIn} input · {totalOut} output
              {totalCachedIn > 0 ? ` · ${totalCachedIn} cached input` : ''}
            </div>
            <div className="mt-2 flex flex-wrap gap-2 text-xs">
              {modelEvents.map((event) => (
                <span key={event.id || event.seq} className="rounded-full bg-muted px-2 py-0.5">
                  {event.model ?? 'unknown model'}{event.provider ? ` · ${event.provider}` : ''}{event.stopReason ? ` · ${event.stopReason}` : ''}
                </span>
              ))}
            </div>
          </div>
        )}

        {contextEvent?.contextWindowSize !== undefined && (
          <div className="rounded-md border p-3 text-sm" aria-label="GenAI context window">
            <div className="font-medium">Context window</div>
            <div className="text-muted-foreground">
              {contextEvent.contextWindowUsed ?? 0} / {contextEvent.contextWindowSize} tokens used
              {contextEvent.contextWindowSize > 0
                ? ` · ${Math.round(((contextEvent.contextWindowUsed ?? 0) / contextEvent.contextWindowSize) * 100)}%`
                : ''}
            </div>
            {(contextEvent.model || contextEvent.provider) && (
              <div className="mt-2 text-xs text-muted-foreground">
                {contextEvent.model ?? 'unknown model'}
                {contextEvent.provider ? ` · ${contextEvent.provider}` : ''}
              </div>
            )}
          </div>
        )}

        <ChildTaskSummary childTasks={task.status?.childTasks} />
      </CardContent>
    </Card>
  )
}
