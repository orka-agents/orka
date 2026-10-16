import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { RefreshCw, Radio, GitFork } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ForkDialog } from '@/components/tasks/fork-dialog'
import type { Task } from '@/schemas/task'

interface RuntimeControlBarProps {
  task: Task
  /** When omitted, the follow toggle is hidden (no live polling to pause). */
  following?: boolean
  onToggleFollow?: () => void
  /** Highest known event seq; the fork dialog checkpoints there. */
  latestSeq?: number
  /** Fork needs execution-event storage; hide it when that's unavailable (501). */
  forkSupported?: boolean
}

/**
 * Production-safe runtime controls mapped to real Orka APIs: read-only
 * refresh and follow, plus fork through the same confirm-and-name dialog the
 * timeline rows use. Delete lives in the task header, visible on every tab.
 */
export function RuntimeControlBar({ task, following, onToggleFollow, latestSeq, forkSupported = true }: RuntimeControlBarProps) {
  const queryClient = useQueryClient()
  // The checkpoint is captured when the dialog opens so later events do not
  // move the fork point the user is confirming.
  const [forkSeq, setForkSeq] = useState<number | null>(null)
  // Keying the dialog by namespace+uid+name gives a route-reused or
  // recreated task a fresh dialog, so a preserved idempotency key can never
  // be replayed against a different task.
  const identity = `${task.metadata.namespace ?? ''}/${task.metadata.uid ?? ''}/${task.metadata.name}`

  return (
    <div className="flex flex-wrap items-center gap-2">
      {onToggleFollow && (
        <Button variant={following ? 'secondary' : 'outline'} size="sm" onClick={onToggleFollow} aria-pressed={following}>
          <Radio className="size-3.5" /> {following ? 'Following' : 'Paused'}
        </Button>
      )}
      <Button variant="outline" size="sm" onClick={() => {
        // The task page reads task + events + trace + approvals + artifacts;
        // refresh all of them, not just the ['tasks'] list, so the visible task
        // data actually updates. Broad prefixes match the namespace/uid-keyed entries.
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
