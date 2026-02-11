import { Link } from '@tanstack/react-router'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import type { Task } from '@/schemas/task'

function elapsed(startTime?: string): string {
  if (!startTime) return ''
  const s = Math.floor((Date.now() - new Date(startTime).getTime()) / 1000)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

const typeBadgeStyles: Record<string, string> = {
  container: 'bg-purple-100 text-purple-800 dark:bg-purple-900 dark:text-purple-200',
  ai: 'bg-indigo-100 text-indigo-800 dark:bg-indigo-900 dark:text-indigo-200',
  agent: 'bg-teal-100 text-teal-800 dark:bg-teal-900 dark:text-teal-200',
}

export function KanbanCard({ task }: { task: Task }) {
  const childCount = task.status?.childTasks?.length ?? 0
  const isRunning = task.status?.phase === 'Running'

  return (
    <Link to="/tasks/$taskId" params={{ taskId: task.metadata.name }} className="block">
      <Card className="gap-0 py-3 hover:shadow-md transition-shadow cursor-pointer">
        <CardContent className="px-3 py-0 space-y-1.5">
          <div className="flex items-center justify-between gap-2">
            <span className="font-semibold text-sm truncate">{task.metadata.name}</span>
            <Badge className={typeBadgeStyles[task.spec.type] ?? ''} variant="secondary">
              {task.spec.type}
            </Badge>
          </div>
          <div className="flex items-center gap-2 text-xs text-muted-foreground flex-wrap">
            <span>{task.metadata.namespace}</span>
            {task.spec.agentRef?.name && (
              <span>agent: {task.spec.agentRef.name}</span>
            )}
            {task.spec.priority != null && task.spec.priority > 0 && (
              <span>pri: {task.spec.priority}</span>
            )}
          </div>
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            {isRunning && task.status?.startTime && (
              <span data-testid="elapsed-time">⏱ {elapsed(task.status.startTime)}</span>
            )}
            {childCount > 0 && (
              <span data-testid="child-count">🔗 {childCount} child{childCount !== 1 ? 'ren' : ''}</span>
            )}
          </div>
        </CardContent>
      </Card>
    </Link>
  )
}
