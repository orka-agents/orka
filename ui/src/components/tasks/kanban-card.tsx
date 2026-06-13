import { Link } from '@tanstack/react-router'
import { Timer, GitBranch } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { phaseStyle, typeStyle } from '@/lib/task-status'
import { cn } from '@/lib/utils'
import type { Task } from '@/schemas/task'

function elapsed(startTime?: string): string {
  if (!startTime) return ''
  const s = Math.floor((Date.now() - new Date(startTime).getTime()) / 1000)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

export function KanbanCard({ task }: { task: Task }) {
  const childCount = task.status?.childTasks?.length ?? 0
  const isRunning = task.status?.phase === 'Running'
  const phase = phaseStyle(task.status?.phase)
  const type = typeStyle(task.spec.type)
  const TypeIcon = type.icon

  return (
    <Link to="/tasks/$taskId" params={{ taskId: task.metadata.name }} className="block">
      <Card
        className={cn('gap-0 py-3 border-l-4 transition-all cursor-pointer hover:shadow-md motion-safe:hover:-translate-y-0.5', phase.railClass)}
      >
        <CardContent className="px-3 py-0 space-y-1.5">
          {/* Phase is encoded by the left-rail color; surface it to AT too. */}
          <span className="sr-only">Phase: {phase.label}</span>
          <div className="flex items-center justify-between gap-2">
            <span className="font-semibold text-sm truncate">{task.metadata.name}</span>
            <Badge className={type.tintClass} variant="secondary">
              <TypeIcon aria-hidden="true" />
              {type.label}
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
              <span data-testid="elapsed-time" className="inline-flex items-center gap-1 tabular-nums">
                <Timer className="size-3" aria-hidden="true" /> {elapsed(task.status.startTime)}
              </span>
            )}
            {childCount > 0 && (
              <span data-testid="child-count" className="inline-flex items-center gap-1">
                <GitBranch className="size-3" aria-hidden="true" /> {childCount} child{childCount !== 1 ? 'ren' : ''}
              </span>
            )}
          </div>
        </CardContent>
      </Card>
    </Link>
  )
}
