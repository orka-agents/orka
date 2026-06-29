import { useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { RefreshCw, Radio, ExternalLink, Trash2, GitFork } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { useDeleteTask } from '@/hooks/use-tasks'
import { useForkTask } from '@/hooks/use-execution-events'
import { isTerminal } from '@/lib/runtime-validation'
import type { Task } from '@/schemas/task'

interface RuntimeControlBarProps {
  task: Task
  /** When omitted, the follow toggle is hidden (no live polling to pause). */
  following?: boolean
  onToggleFollow?: () => void
  /** Highest known event seq; pins fork afterSeq so retries stay idempotent. */
  latestSeq?: number
  /** Fork needs execution-event storage; hide it when that's unavailable (501). */
  forkSupported?: boolean
}

/**
 * Production-safe runtime controls mapped to real Orka APIs. Read-only/UI-only
 * actions (refresh, follow, open) plus destructive (delete) and fork, both of
 * which preserve controller ownership. Destructive delete requires confirmation.
 * No Step/Run/Inject — those are dev-simulator only.
 */
export function RuntimeControlBar({ task, following, onToggleFollow, latestSeq, forkSupported = true }: RuntimeControlBarProps) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const del = useDeleteTask()
  const fork = useForkTask(task.metadata.name)
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)
  // Pinned fork retry state, tagged with the source task so a route reuse for a
  // different task can't send the previous task's key/seq. Cleared on success.
  const forkRef = useRef<{ source: string; key: string; seq?: number } | null>(null)
  const terminal = isTerminal(task.status?.phase)
  // Scope the armed confirm to namespace+uid+name so a namespace switch or
  // same-name/new-uid recreation drops a stale confirm rather than deleting a
  // different task.
  const identity = `${task.metadata.namespace ?? ''}/${task.metadata.uid ?? ''}/${task.metadata.name}`
  const deleteArmed = confirmDelete === identity

  return (
    <div className="flex flex-wrap items-center gap-2">
      {onToggleFollow && (
        <Button variant={following ? 'secondary' : 'outline'} size="sm" onClick={onToggleFollow} aria-pressed={following}>
          <Radio className="size-3.5" /> {following ? 'Following' : 'Paused'}
        </Button>
      )}
      <Button variant="outline" size="sm" onClick={() => {
        // The runtime view reads task + events + trace + approvals + artifacts;
        // refresh all of them, not just the ['tasks'] list, so the visible task
        // data actually updates. Broad prefixes match the namespace/uid-keyed entries.
        const id = task.metadata.name
        for (const key of [['tasks'], ['task', id], ['taskEvents', id], ['taskEventsPage', id], ['taskTrace', id], ['taskApprovals', id], ['taskArtifacts', id]]) {
          queryClient.invalidateQueries({ queryKey: key })
        }
      }}>
        <RefreshCw className="size-3.5" /> Refresh
      </Button>
      <Button variant="outline" size="sm" onClick={() => navigate({ to: '/tasks/$taskId', params: { taskId: task.metadata.name } })}>
        <ExternalLink className="size-3.5" /> Open
      </Button>
      {forkSupported && (
        <Button
          variant="outline"
          size="sm"
          disabled={fork.isPending || latestSeq === undefined}
          title={latestSeq === undefined ? 'Fork available once events load' : undefined}
          onClick={async () => {
            // One pinned key+seq per logical fork, tagged with the task identity
            // (namespace+uid+name). Reset if it belongs to a different task
            // (route reuse, namespace switch, or same-name/new-uid recreation)
            // or was cleared after success; stable across this task's own retries.
            if (!forkRef.current || forkRef.current.source !== identity) {
              forkRef.current = {
                source: identity,
                key: globalThis.crypto?.randomUUID?.() ?? `fork-${Date.now()}-${Math.round(Math.random() * 1e9)}`,
                seq: latestSeq,
              }
            }
            try {
              const res = await fork.mutateAsync({ idempotencyKey: forkRef.current.key, afterSeq: forkRef.current.seq })
              forkRef.current = null
              toast.success(`Forked to ${res.newTaskName}`)
              navigate({ to: '/tasks/$taskId', params: { taskId: res.newTaskName } })
            } catch {
              toast.error('Fork failed')
            }
          }}
        >
          <GitFork className="size-3.5" /> Fork
        </Button>
      )}
      {deleteArmed ? (
        <span className="flex items-center gap-1">
          <Button variant="destructive" size="sm" onClick={async () => { await del.mutateAsync(task.metadata.name); toast.success('Task deleted'); navigate({ to: '/tasks' }) }}>
            Confirm delete
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setConfirmDelete(null)}>Cancel</Button>
        </span>
      ) : (
        <Button variant="ghost" size="sm" onClick={() => setConfirmDelete(identity)} title={terminal ? 'Delete task' : 'Delete (cancels a running task)'}>
          <Trash2 className="size-3.5" /> Delete
        </Button>
      )}
    </div>
  )
}
