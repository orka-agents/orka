import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Workflow } from 'lucide-react'
import { ExecutionGraph } from '@/components/tasks/execution-graph'
import { isLiveTask } from '@/lib/runtime-activity'
import type { Task } from '@/schemas/task'
import type { ExecutionEvent } from '@/schemas/execution-event'

interface TaskFlowPanelProps {
  /** Selected root task; when present, renders root + child graph. */
  task?: Task | null
  /** Fallback list when no root is selected (e.g. namespace view). */
  tasks?: Task[]
  events?: ExecutionEvent[]
}

/**
 * Runtime-facing wrapper over <ExecutionGraph>. Three scopes:
 *  1. single selected task (root + children),
 *  2. selected root with child tasks (graph handles it),
 *  3. fallback: list running/recent tasks each as a one-node graph.
 * Dependency-free; degrades to an empty state when nothing is selectable.
 */
export function TaskFlowPanel({ task, tasks, events = [] }: TaskFlowPanelProps) {
  const fallback = (tasks ?? []).filter(isLiveTask).slice(0, 8)

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-medium">Task flow</CardTitle>
      </CardHeader>
      <CardContent className="pt-0">
        {task ? (
          <ExecutionGraph task={task} events={events} />
        ) : fallback.length > 0 ? (
          <div className="space-y-2">
            {fallback.map((t) => (
              <ExecutionGraph key={t.metadata.uid || t.metadata.name} task={t} />
            ))}
          </div>
        ) : (
          <EmptyState icon={Workflow} headline="No active flow" hint="Running tasks and their children appear here." className="py-6" />
        )}
      </CardContent>
    </Card>
  )
}
