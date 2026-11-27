import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { RefreshCw, Radio, GitFork } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ForkDialog } from '@/components/tasks/fork-dialog'
import type { Task } from '@/schemas/task'

interface RuntimeControlBarProps {
  task: Task
  following?: boolean
  onToggleFollow?: () => void
  latestSeq?: number
  forkSupported?: boolean
}

export function RuntimeControlBar({ task, following, onToggleFollow, latestSeq, forkSupported = true }: RuntimeControlBarProps) {
  const queryClient = useQueryClient()
  const [forkSeq, setForkSeq] = useState<number | null>(null)
  const identity = `${task.metadata.namespace ?? ''}/${task.metadata.uid ?? ''}/${task.metadata.name}`

  return (
    <div className="flex flex-wrap items-center gap-2">
      {onToggleFollow && (
        <Button variant={following ? 'secondary' : 'outline'} size="sm" onClick={onToggleFollow} aria-pressed={following}>
          <Radio className="size-3.5" /> {following ? 'Following' : 'Paused'}
        </Button>
      )}
      <Button variant="outline" size="sm" onClick={() => {
        const id = task.metadata.name
        for (const key of [['tasks'], ['task', id], ['taskEvents', id], ['taskEventsPage', id], ['taskTrace', id], ['taskApprovals', id], ['taskArtifacts', id]]) {
          queryClient.invalidateQueries({ queryKey: key })
        }
      }}>
        <RefreshCw className="size-3.5" /> Refresh
      </Button>
      {forkSupported && (
        <>
          <Button
            variant="outline"
            size="sm"
            disabled={latestSeq === undefined}
            title={latestSeq === undefined ? 'Fork available once events load' : 'Fork from the latest event'}
            onClick={() => setForkSeq(latestSeq ?? null)}
          >
            <GitFork className="size-3.5" /> Fork
          </Button>
          <ForkDialog
            key={identity}
            taskId={task.metadata.name}
            event={null}
            afterSeq={forkSeq ?? 0}
            open={forkSeq !== null}
            onOpenChange={(open) => {
              if (!open) setForkSeq(null)
            }}
          />
        </>
      )}
    </div>
  )
}
