import { Link } from '@tanstack/react-router'
import { ShieldQuestion } from 'lucide-react'

import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { TaskStatusBadge } from '@/components/tasks/task-status-badge'
import type { Task } from '@/schemas/task'

function isWaitingForApproval(task: Task): boolean {
  return (task.status?.conditions ?? []).some(
    (condition) => condition.type === 'WaitingForApproval' && condition.status === 'True',
  )
}

// Cross-task approval inbox: tasks parked on the WaitingForApproval
// condition, linked straight to their approvals tab.
export function NeedsAttention({ tasks }: { tasks: Task[] }) {
  const waiting = tasks.filter(isWaitingForApproval)
  if (waiting.length === 0) return null

  return (
    <Card className="border-status-pending/40">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <ShieldQuestion className="h-4 w-4 text-status-pending" />
          Waiting for approval
          <span className="rounded-full bg-status-pending-bg px-2 text-xs font-semibold text-status-pending">
            {waiting.length}
          </span>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {waiting.slice(0, 6).map((task) => (
          <Link
            key={task.metadata.uid ?? task.metadata.name}
            to="/tasks/$taskId"
            params={{ taskId: task.metadata.name }}
            search={{ tab: 'approvals' }}
            className="flex items-center justify-between gap-2 rounded-md border px-3 py-2 text-sm transition-colors hover:bg-accent"
          >
            <span className="truncate font-medium">{task.metadata.name}</span>
            <span className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
              {task.spec.agentRef?.name && <span>{task.spec.agentRef.name}</span>}
              <TaskStatusBadge phase={task.status?.phase} />
            </span>
          </Link>
        ))}
        {waiting.length > 6 && (
          <p className="text-xs text-muted-foreground">And {waiting.length - 6} more — see Tasks.</p>
        )}
      </CardContent>
    </Card>
  )
}
