import { useState, useEffect } from 'react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { TaskStatusBadge } from './task-status-badge'
import type { Task } from '@/schemas/task'

const steps = ['Pending', 'Running', 'Completed'] as const

// Map a task phase onto the coarse Pending → Running → Completed stepper.
// Scheduled hasn't started yet, so it sits at the start (step 0) rather than
// being mistaken for Completed. Terminal phases (Succeeded/Failed/Cancelled)
// land on the final step — the run has ended.
function stepIndex(phase?: string): number {
  if (!phase || phase === 'Pending' || phase === 'Scheduled') return 0
  if (phase === 'Running') return 1
  return 2
}

function ElapsedTime({ startTime, completionTime }: { startTime?: string; completionTime?: string }) {
  const [elapsed, setElapsed] = useState(() => startTime ? '' : '-')

  useEffect(() => {
    if (!startTime) return
    const start = new Date(startTime).getTime()

    function update() {
      const end = completionTime ? new Date(completionTime).getTime() : Date.now()
      const s = Math.floor((end - start) / 1000)
      if (s < 60) setElapsed(`${s}s`)
      else if (s < 3600) setElapsed(`${Math.floor(s / 60)}m ${s % 60}s`)
      else setElapsed(`${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`)
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
  const running = childTasks.filter(c => c.phase === 'Running').length
  const succeeded = childTasks.filter(c => c.phase === 'Succeeded').length
  const failed = childTasks.filter(c => c.phase === 'Failed').length
  const pending = childTasks.filter(c => c.phase === 'Pending').length

  return (
    <div data-testid="child-task-summary">
      <span className="text-muted-foreground text-sm">Child Tasks: </span>
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

export function TaskExecutionPanel({ task }: { task: Task }) {
  const phase = task.status?.phase
  const current = stepIndex(phase)

  return (
    <Card>
      <CardHeader><CardTitle>Execution</CardTitle></CardHeader>
      <CardContent className="space-y-6">
        {/* Progress steps */}
        <div className="flex items-center gap-2" data-testid="progress-steps">
          {steps.map((step, i) => (
            <div key={step} className="flex items-center gap-2">
              <div className={`flex h-8 w-8 items-center justify-center rounded-full text-xs font-medium ${
                i < current
                  ? 'bg-status-succeeded-bg text-status-succeeded'
                  : i === current
                  ? 'bg-status-running-bg text-status-running'
                  : 'bg-muted text-muted-foreground'
              }`}>
                {i < current ? '✓' : i + 1}
              </div>
              <span className={`text-sm ${i <= current ? 'font-medium' : 'text-muted-foreground'}`}>{step}</span>
              {i < steps.length - 1 && (
                <div className={`h-0.5 w-8 ${i < current ? 'bg-status-succeeded' : 'bg-muted'}`} />
              )}
            </div>
          ))}
        </div>

        {/* Status details */}
        <div className="grid gap-3 text-sm md:grid-cols-2">
          <div className="flex items-center gap-2">
            <span className="text-muted-foreground">Phase:</span>
            <TaskStatusBadge phase={phase} />
          </div>
          <div className="flex items-center gap-2">
            <span className="text-muted-foreground">Elapsed:</span>
            <ElapsedTime startTime={task.status?.startTime} completionTime={task.status?.completionTime} />
          </div>
          <div>
            <span className="text-muted-foreground">Attempts:</span>{' '}
            {task.status?.attempts ?? 0}
          </div>
          {task.status?.message && (
            <div className="md:col-span-2">
              <span className="text-muted-foreground">Message:</span>{' '}
              {task.status.message}
            </div>
          )}
        </div>

        <ChildTaskSummary childTasks={task.status?.childTasks} />
      </CardContent>
    </Card>
  )
}
