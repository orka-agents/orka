import { Link } from '@tanstack/react-router'
import { Bot } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { StatusDot } from '@/components/ui/status-dot'
import { EmptyState } from '@/components/ui/empty-state'
import { cn } from '@/lib/utils'
import {
  groupTasksByAgent,
  taskKey,
  UNASSIGNED_AGENT,
  type LatestEventSeqByTask,
} from '@/lib/runtime-activity'
import type { Task } from '@/schemas/task'

interface AgentsRosterProps {
  tasks: Task[]
  /** Name of the task currently spotlit, for highlight. */
  activeTaskName?: string
  latestSeq?: LatestEventSeqByTask
}

/**
 * Running/recent tasks grouped by owning agent, with an "unassigned" bucket for
 * agent-less tasks. The spotlit task's row is highlighted with the live rail.
 */
export function AgentsRoster({ tasks, activeTaskName, latestSeq }: AgentsRosterProps) {
  const groups = groupTasksByAgent(tasks, latestSeq)

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-medium">Agents</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 pt-0">
        {groups.length === 0 ? (
          <EmptyState
            icon={Bot}
            headline="No agents active"
            hint="Agents appear here while their tasks run in this namespace."
            className="py-6"
          />
        ) : (
          groups.map((group) => (
            <div key={group.agent} className="space-y-1.5">
              <div className="flex items-center justify-between">
                <span className="font-mono text-xs font-medium">
                  {group.agent === UNASSIGNED_AGENT ? 'Unassigned' : group.agent}
                </span>
                <Badge variant="secondary">{group.tasks.length}</Badge>
              </div>
              <ul className="space-y-1">
                {group.tasks.map((task) => (
                  <li key={taskKey(task)}>
                    <Link
                      to="/tasks/$taskId"
                      params={{ taskId: task.metadata.name }}
                      className={cn(
                        'flex items-center justify-between gap-2 rounded-md border-l-2 border-transparent py-1 pl-2 pr-1 hover:bg-accent',
                        task.metadata.name === activeTaskName && 'border-l-live bg-accent/50',
                      )}
                    >
                      <span className="truncate font-mono text-xs">{task.metadata.name}</span>
                      <StatusDot phase={task.status?.phase} hideLabel />
                    </Link>
                  </li>
                ))}
              </ul>
            </div>
          ))
        )}
      </CardContent>
    </Card>
  )
}
