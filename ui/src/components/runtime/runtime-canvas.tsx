import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { RefreshCw, Radio } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { SonarPing } from '@/components/ui/sonar-ping'
import { EmptyState } from '@/components/ui/empty-state'
import { PageHeader } from '@/components/layout/page-header'
import { StatusDot } from '@/components/ui/status-dot'
import { useTaskList, useTaskEvents } from '@/hooks/use-tasks'
import { useUIStore } from '@/stores/ui'
import { isLiveTask, selectActiveTask } from '@/lib/runtime-activity'
import type { Task } from '@/schemas/task'
import { ActivitySpotlight } from './activity-spotlight'
import { AgentsRoster } from './agents-roster'
import { TaskFlowPanel } from './task-flow-panel'

/**
 * Spotlight bound to the active task's latest execution event. Fetching events
 * for the single spotlit task (not every row) keeps the namespace view cheap
 * while still surfacing "latest event summary" — the Phase 2 criterion. Polls
 * only while following; falls back silently to status.message when events are
 * unavailable (501/empty).
 */
function ActiveSpotlight({ task, following }: { task: Task | null; following: boolean }) {
  const { data } = useTaskEvents(
    task?.metadata.name ?? '',
    following ? 5000 : false,
    task?.metadata.uid,
  )
  const latestEvent = data?.events[data.events.length - 1]
  return <ActivitySpotlight task={task} latestEvent={latestEvent} following={following} />
}

/**
 * Orka-native Runtime Canvas (Slice A): a read-only operator view of agent/task
 * execution backed entirely by real Orka Tasks. Slice A is namespace-scoped and
 * exposes no mutating controls beyond refresh and a follow toggle. The store's
 * task source is the only truth; nothing here is synthetic or editable.
 */
export function RuntimeCanvas() {
  const namespace = useUIStore((s) => s.namespace)
  const queryClient = useQueryClient()
  const [following, setFollowing] = useState(true)
  const { data, isLoading } = useTaskList('25', following ? 10000 : false)

  const tasks = data?.items ?? []
  const runningTasks = tasks.filter(isLiveTask)
  const active = selectActiveTask(tasks)
  // remainingItemCount is best-effort/optional and is nulled under token
  // filtering; a continue token alone still means more pages exist.
  const truncated =
    (data?.metadata?.remainingItemCount ?? 0) > 0 || Boolean(data?.metadata?.continue)

  const refreshCanvas = () => {
    queryClient.invalidateQueries({ queryKey: ['tasks'] })
    if (active) {
      queryClient.invalidateQueries({
        queryKey: ['taskEvents', active.metadata.name, namespace, active.metadata.uid ?? ''],
      })
    }
  }

  const controls = (
    <div className="flex items-center gap-2">
      <Button
        variant={following ? 'secondary' : 'outline'}
        size="sm"
        onClick={() => setFollowing((f) => !f)}
        aria-pressed={following}
      >
        <Radio className="size-3.5" />
        {following ? 'Following' : 'Paused'}
      </Button>
      <Button
        variant="outline"
        size="sm"
        onClick={refreshCanvas}
      >
        <RefreshCw className="size-3.5" />
        Refresh
      </Button>
    </div>
  )

  return (
    <div className="space-y-4">
      <PageHeader
        title="Runtime Canvas"
        description={`${runningTasks.length} active · namespace ${namespace}`}
        action={controls}
      />

      {isLoading ? (
        <div className="grid gap-4 lg:grid-cols-3">
          <Skeleton className="h-40 w-full lg:col-span-2" />
          <Skeleton className="h-40 w-full" />
        </div>
      ) : tasks.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-4 py-12">
            <SonarPing />
            <EmptyState
              headline={`No tasks in namespace "${namespace}"`}
              hint="This view shows only the selected namespace. Running agents appear here as tasks start."
              className="py-0"
            />
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 lg:grid-cols-3">
          <div className="space-y-4 lg:col-span-2">
            <ActiveSpotlight task={active} following={following} />
            <TaskFlowPanel task={active} tasks={runningTasks} />
          </div>
          <AgentsRoster tasks={runningTasks} activeTaskName={active?.metadata.name} />
        </div>
      )}

      {truncated && (
        <p className="text-xs text-muted-foreground">
          <StatusDot phase="Pending" hideLabel /> Showing the first {tasks.length} tasks in “{namespace}”; more exist.
        </p>
      )}
    </div>
  )
}
