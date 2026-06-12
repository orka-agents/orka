import { cn } from '@/lib/utils'
import { CheckCircle2, Circle, XCircle, Flag } from 'lucide-react'
import type { Task, PlanState } from '@/schemas/task'

interface TimelineEvent {
  key: string
  label: string
  detail?: string
  /** ISO timestamp used for chronological ordering (optional). */
  at?: string
  /**
   * Ordering bucket. Everything shares bucket 0 and sorts purely by timestamp;
   * the terminal marker is pinned to bucket 1 so it always renders last, even
   * when an earlier event is undated or its completion time precedes a
   * still-running synthetic event.
   */
  bucket: number
  status: 'done' | 'active' | 'pending' | 'failed'
}

/** A condition as carried on task.status.conditions[]. */
interface Condition {
  type: string
  status: string
  reason?: string
  message?: string
  lastTransitionTime?: string
}

/**
 * Derive an ordered list of timeline events from a task's conditions and
 * iteration. Events sort chronologically by timestamp; the synthetic
 * "Iteration N" event is anchored to the latest known time so it slots after
 * the work that preceded it, and the terminal marker is always pinned last.
 */
function buildEvents(task: Task, plan?: PlanState): TimelineEvent[] {
  const events: TimelineEvent[] = []
  const phase = task.status?.phase

  events.push({
    key: 'created',
    label: 'Created',
    at: task.metadata.creationTimestamp,
    bucket: 0,
    status: 'done',
  })

  if (task.status?.startTime) {
    events.push({
      key: 'started',
      label: 'Started',
      at: task.status.startTime,
      bucket: 0,
      status: 'done',
    })
  }

  // Merge status conditions as timestamped events.
  const conditions = (task.status?.conditions ?? []) as Condition[]
  const conditionTimes: number[] = []
  for (const c of conditions) {
    if (c.lastTransitionTime) {
      const t = new Date(c.lastTransitionTime).getTime()
      if (!Number.isNaN(t)) conditionTimes.push(t)
    }
    events.push({
      key: `cond-${c.type}`,
      label: c.type,
      detail: c.message ?? c.reason,
      at: c.lastTransitionTime,
      bucket: 0,
      status: c.status === 'True' ? 'done' : 'pending',
    })
  }

  // The current iteration, with the plan summary as its detail. Anchor it to
  // the latest known activity time (newest condition, else startTime) so it
  // sorts after the work that led to it rather than floating to the end.
  const iteration = task.status?.iteration ?? 0
  if (iteration > 0) {
    const terminal = phase === 'Succeeded' || phase === 'Failed'
    const startMs = task.status?.startTime ? new Date(task.status.startTime).getTime() : NaN
    const anchorMs = Math.max(
      ...conditionTimes,
      Number.isNaN(startMs) ? -Infinity : startMs,
    )
    events.push({
      key: `iteration-${iteration}`,
      label: `Iteration ${iteration}`,
      detail: plan?.summary,
      at: Number.isFinite(anchorMs) ? new Date(anchorMs).toISOString() : undefined,
      bucket: 0,
      status: terminal ? 'done' : 'active',
    })
  }

  // Terminal marker — pinned to the last bucket so it always renders last,
  // with phase-correct styling.
  if (phase === 'Succeeded' || phase === 'Failed') {
    events.push({
      key: 'terminal',
      label: phase,
      at: task.status?.completionTime,
      bucket: 1,
      status: phase === 'Failed' ? 'failed' : 'done',
    })
  }

  // Sort by bucket (terminal last), then chronologically within a bucket;
  // undated events keep insertion order via the stable index tiebreak.
  return events
    .map((e, i) => ({ e, i }))
    .sort((a, b) => {
      if (a.e.bucket !== b.e.bucket) return a.e.bucket - b.e.bucket
      const ta = a.e.at ? new Date(a.e.at).getTime() : Number.NaN
      const tb = b.e.at ? new Date(b.e.at).getTime() : Number.NaN
      if (Number.isNaN(ta) && Number.isNaN(tb)) return a.i - b.i
      if (Number.isNaN(ta)) return 1
      if (Number.isNaN(tb)) return -1
      if (ta === tb) return a.i - b.i
      return ta - tb
    })
    .map(({ e }) => e)
}

function EventIcon({ status }: { status: TimelineEvent['status'] }) {
  if (status === 'failed')
    return <XCircle className="size-4 text-status-failed" aria-hidden="true" />
  if (status === 'done')
    return <CheckCircle2 className="size-4 text-status-succeeded" aria-hidden="true" />
  if (status === 'active')
    return (
      <Circle className="size-4 text-status-running motion-safe:animate-pulse-live" aria-hidden="true" />
    )
  return <Circle className="size-4 text-muted-foreground" aria-hidden="true" />
}

interface RunTimelineProps {
  task: Task
  plan?: PlanState
  className?: string
}

/**
 * Autonomous-loop timeline — renders a run as a plan→act→re-plan→converge
 * story instead of a lone "Iteration: 4" metadata field.
 *
 * Vertical stepper: Created → Started → (conditions, by lastTransitionTime) →
 * Iteration N (plan summary) → terminal (Succeeded/Failed), with a goal
 * progress bar carrying iteration tick marks and explicit goal-complete styling.
 *
 * Handles a missing `plan` gracefully (falls back to a conditions-only
 * timeline). Intended to be gated by the caller on `iteration > 0`.
 */
export function RunTimeline({ task, plan, className }: RunTimelineProps) {
  const events = buildEvents(task, plan)
  const iteration = task.status?.iteration ?? 0
  const progressPct = plan?.progressPct
  const goalComplete = plan?.goalComplete ?? false

  return (
    <div className={cn('space-y-4', className)}>
      {progressPct !== undefined && (
        <div className="space-y-1">
          <div className="flex items-center justify-between text-sm">
            <span className="flex items-center gap-1.5 font-medium">
              {goalComplete && <Flag className="size-3.5 text-status-succeeded" aria-hidden="true" />}
              {goalComplete ? 'Goal complete' : 'Progress toward goal'}
            </span>
            <span className="tabular-nums text-muted-foreground">{progressPct}%</span>
          </div>
          <div
            className="relative h-2 overflow-hidden rounded-full bg-muted"
            role="progressbar"
            aria-valuenow={progressPct}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-label="Goal progress"
          >
            <div
              className={cn(
                'h-full rounded-full transition-all',
                goalComplete ? 'bg-status-succeeded' : 'bg-primary',
              )}
              style={{ width: `${Math.min(100, Math.max(0, progressPct))}%` }}
            />
            {/* Iteration tick marks. */}
            {iteration > 1 &&
              Array.from({ length: iteration - 1 }).map((_, i) => (
                <span
                  key={i}
                  className="absolute top-0 h-full w-px bg-background/60"
                  style={{ left: `${((i + 1) / iteration) * 100}%` }}
                  aria-hidden="true"
                />
              ))}
          </div>
        </div>
      )}

      <ol className="space-y-0">
        {events.map((e, i) => (
          <li key={e.key} className="flex gap-3">
            <div className="flex flex-col items-center">
              <EventIcon status={e.status} />
              {i < events.length - 1 && <span className="w-px flex-1 bg-border" aria-hidden="true" />}
            </div>
            <div className={cn('pb-4', i === events.length - 1 && 'pb-0')}>
              <p
                className={cn(
                  'text-sm font-medium',
                  e.status === 'active' && 'text-status-running',
                  e.status === 'failed' && 'text-status-failed',
                )}
              >
                {e.label}
              </p>
              {e.detail && <p className="text-xs text-muted-foreground">{e.detail}</p>}
            </div>
          </li>
        ))}
      </ol>
    </div>
  )
}
