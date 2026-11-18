import { useState } from 'react'
import type { TaskPhase } from '@/schemas/task'
import { TaskEventTimeline } from './task-event-timeline'
import { TaskTracePanel } from './task-trace-panel'
import { ViewSwitch } from './view-switch'

export type ActivityView = 'trace' | 'events'

const ACTIVITY_VIEWS = [
  { value: 'trace', label: 'Trace' },
  { value: 'events', label: 'Raw events' },
] as const

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
      <ViewSwitch label="Activity view" value={view} options={ACTIVITY_VIEWS} onChange={setView} />
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
