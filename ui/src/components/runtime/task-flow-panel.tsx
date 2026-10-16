import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Workflow } from 'lucide-react'
import { ExecutionGraph } from '@/components/tasks/execution-graph'
import type { Task } from '@/schemas/task'
import type { ExecutionEvent } from '@/schemas/execution-event'

interface TaskFlowPanelProps {
  /** The task to draw with its children. */
  task?: Task | null
  events?: ExecutionEvent[]
}

/** Runtime-facing composition of <ExecutionGraph>: the task with its children. */
export function TaskFlowPanel({ task, events = [] }: TaskFlowPanelProps) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-medium">Task flow</CardTitle>
      </CardHeader>
      <CardContent className="pt-0">
        {task ? (
          <ExecutionGraph task={task} events={events} />
        ) : (
          <EmptyState icon={Workflow} headline="No active flow" hint="Running tasks and their children appear here." className="py-6" />
        )}
      </CardContent>
    </Card>
  )
}
