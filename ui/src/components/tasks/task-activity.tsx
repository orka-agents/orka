import { useState } from 'react'
import { Button } from '@/components/ui/button'
import type { TaskPhase } from '@/schemas/task'
import { TaskEventTimeline } from './task-event-timeline'
import { TaskTracePanel } from './task-trace-panel'

export type ActivityView = 'trace' | 'events'

interface TaskActivityProps {
  taskId: string
  taskUid?: string
  taskNamespace?: string
  taskPhase?: TaskPhase
  initialView?: ActivityView
}

/**
 * What the task did, step by step: the same execution events grouped into a
 * trace, or as the raw stream with filters and "Fork from here".
 */
export function TaskActivity({ taskId, taskUid, taskNamespace, taskPhase, initialView = 'trace' }: TaskActivityProps) {
  const [view, setView] = useState<ActivityView>(initialView)
  return (
    <div className="space-y-3">
      <div role="group" aria-label="Activity view" className="inline-flex gap-1 rounded-md border p-0.5">
        <Button size="sm" variant={view === 'trace' ? 'secondary' : 'ghost'} aria-pressed={view === 'trace'} onClick={() => setView('trace')}>
          Trace
        </Button>
        <Button size="sm" variant={view === 'events' ? 'secondary' : 'ghost'} aria-pressed={view === 'events'} onClick={() => setView('events')}>
          Raw events
        </Button>
      </div>
      {view === 'trace' ? (
        <TaskTracePanel taskId={taskId} taskUid={taskUid} />
      ) : (
        <TaskEventTimeline
          key={`${taskNamespace ?? ''}/${taskId}/${taskUid ?? ''}`}
          taskId={taskId}
          taskPhase={taskPhase}
          taskUid={taskUid}
        />
      )}
    </div>
  )
}
