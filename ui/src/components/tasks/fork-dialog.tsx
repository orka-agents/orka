import { useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { GitFork, ArrowRight } from 'lucide-react'
import { useForkTask } from '@/hooks/use-execution-events'
import { ApiError } from '@/lib/api-client'
import type { ExecutionEvent, ForkTaskResponse } from '@/schemas/execution-event'

export interface ForkDialogProps {
  taskId: string
  // The event row the fork was launched from. Its seq seeds afterSeq.
  event: ExecutionEvent | null
  open: boolean
  onOpenChange: (open: boolean) => void
}

// Stable random id for an idempotency key. crypto.randomUUID is available in all
// supported browsers and the test environment.
function newIdempotencyKey(): string {
  return globalThis.crypto?.randomUUID?.() ?? `fork-${Date.now()}-${Math.round(Math.random() * 1e9)}`
}

export function ForkDialog({ taskId, event, open, onOpenChange }: ForkDialogProps) {
  const fork = useForkTask(taskId)
  const [newTaskName, setNewTaskName] = useState('')
  const [agentName, setAgentName] = useState('')
  const [prompt, setPrompt] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [created, setCreated] = useState<ForkTaskResponse | null>(null)
  // Bumped on every reset so an in-flight submission that resolves after the
  // dialog was closed (via Escape/overlay/X while pending) does not repopulate
  // state on the now-closed, mounted component.
  const submissionRef = useRef(0)
  // A stable idempotency key per logical submission, reused across retries from
  // the error state so a network drop after the server created the fork resolves
  // to the same task instead of minting a duplicate. Regenerated on reset.
  const idempotencyKeyRef = useRef<string>('')
  // The checkpoint (afterSeq) the current key was minted for. The backend derives
  // the deterministic auto-name solely from the key, so a key reused for a
  // different checkpoint would collide with the earlier fork; bind the key to its
  // afterSeq and mint a fresh one when the checkpoint changes.
  const idempotencyKeySeqRef = useRef<number | null>(null)

  const afterSeq = event?.seq ?? 0

  function reset() {
    submissionRef.current += 1
    // Preserve the idempotency key while a submission is still in flight: the
    // POST can't be aborted, so if it succeeds server-side after the dialog was
    // dismissed (Escape/overlay/X), a retry of the same blank-name fork must
    // reuse this key so the backend collapses the duplicate instead of minting a
    // second task. Once the mutation has settled, clear it normally.
    if (!fork.isPending) {
      idempotencyKeyRef.current = ''
      idempotencyKeySeqRef.current = null
    }
    setNewTaskName('')
    setAgentName('')
    setPrompt('')
    setError(null)
    setCreated(null)
    fork.reset()
  }

  async function submit() {
    if (fork.isPending) return
    setError(null)
    const submission = submissionRef.current
    // Reuse the key across retries of this submission, but mint a fresh one if it
    // was minted for a different checkpoint (e.g. a key preserved from a pending
    // close on another event) — the backend's deterministic auto-name derives
    // from the key alone, so reusing it for a different afterSeq would collide.
    if (!idempotencyKeyRef.current || idempotencyKeySeqRef.current !== afterSeq) {
      idempotencyKeyRef.current = newIdempotencyKey()
      idempotencyKeySeqRef.current = afterSeq
    }
    try {
      const result = await fork.mutateAsync({
        afterSeq,
        newTaskName: newTaskName.trim() || undefined,
        agentRef: agentName.trim() ? { name: agentName.trim() } : undefined,
        prompt: prompt.trim() || undefined,
        idempotencyKey: idempotencyKeyRef.current,
      })
      // Ignore the result if the dialog was reset/closed while in flight.
      if (submission !== submissionRef.current) return
      setCreated(result)
    } catch (err) {
      if (submission !== submissionRef.current) return
      if (err instanceof ApiError) {
        setError(err.message || `Fork failed (${err.status}).`)
      } else {
        setError(err instanceof Error ? err.message : 'Fork failed.')
      }
    }
  }

  // Editing any fork parameter invalidates the current idempotency key. If a
  // prior attempt actually created the fork (an ambiguous failure), reusing the
  // key after changing prompt/agent/name would resolve to that original task and
  // silently ignore the new overrides; minting a fresh key on the next submit
  // makes the edited fork a distinct request.
  function onEdit<T>(setter: (value: T) => void) {
    return (value: T) => {
      idempotencyKeyRef.current = ''
      setter(value)
    }
  }
  const setNewTaskNameEdited = onEdit(setNewTaskName)
  const setAgentNameEdited = onEdit(setAgentName)
  const setPromptEdited = onEdit(setPrompt)

  // Single close path so every dismissal — Escape, overlay, the X button, and the
  // footer Close/Cancel buttons — resets form and result state. The dialog stays
  // mounted under TaskEventTimeline, so skipping reset would reopen on a stale
  // success screen or leftover input the next time a row is forked.
  function handleOpenChange(next: boolean) {
    if (!next) reset()
    onOpenChange(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <GitFork className="h-4 w-4" /> Fork from checkpoint
          </DialogTitle>
          <DialogDescription>
            Create a new task that replays this task's context through sequence{' '}
            <span className="font-mono">#{afterSeq}</span>.
          </DialogDescription>
        </DialogHeader>

        {created ? (
          <div className="space-y-3">
            <p className="text-sm">
              Forked task created from <span className="font-mono">#{created.afterSeq}</span>.
            </p>
            <Link
              to="/tasks/$taskId"
              params={{ taskId: created.newTaskName }}
              onClick={() => handleOpenChange(false)}
              className="inline-flex items-center gap-1 rounded-md border bg-card px-3 py-2 text-sm font-medium text-primary hover:underline"
            >
              {created.newTaskName} <ArrowRight className="h-4 w-4" />
            </Link>
            {created.forkContext?.truncated && (
              <p className="text-xs text-muted-foreground">
                The forked context was truncated to a bounded window of prior events.
              </p>
            )}
          </div>
        ) : (
          <div className="space-y-3">
            <div className="space-y-1">
              <label htmlFor="fork-name" className="text-xs font-medium text-muted-foreground">
                New task name (optional)
              </label>
              <input
                id="fork-name"
                type="text"
                value={newTaskName}
                onChange={(e) => setNewTaskNameEdited(e.target.value)}
                placeholder="auto-generated if blank"
                className="h-9 w-full rounded-md border bg-background px-3 text-sm"
                disabled={fork.isPending}
              />
            </div>
            <div className="space-y-1">
              <label htmlFor="fork-agent" className="text-xs font-medium text-muted-foreground">
                Agent override (optional)
              </label>
              <input
                id="fork-agent"
                type="text"
                value={agentName}
                onChange={(e) => setAgentNameEdited(e.target.value)}
                placeholder="keep source agent if blank"
                className="h-9 w-full rounded-md border bg-background px-3 text-sm"
                disabled={fork.isPending}
              />
            </div>
            <div className="space-y-1">
              <label htmlFor="fork-prompt" className="text-xs font-medium text-muted-foreground">
                Prompt override / continuation (optional)
              </label>
              <textarea
                id="fork-prompt"
                value={prompt}
                onChange={(e) => setPromptEdited(e.target.value)}
                placeholder="keep source prompt if blank"
                rows={3}
                className="w-full rounded-md border bg-background px-3 py-2 text-sm"
                disabled={fork.isPending}
              />
            </div>
            {error && (
              <p className="rounded-md border border-status-failed/40 bg-status-failed-bg px-3 py-2 text-xs text-status-failed">
                {error}
              </p>
            )}
          </div>
        )}

        <DialogFooter>
          {created ? (
            <Button variant="outline" onClick={() => handleOpenChange(false)}>Close</Button>
          ) : (
            <>
              <Button variant="outline" onClick={() => handleOpenChange(false)} disabled={fork.isPending}>
                Cancel
              </Button>
              <Button onClick={submit} disabled={fork.isPending}>
                <GitFork className="mr-1 h-4 w-4" />
                {fork.isPending ? 'Forking…' : 'Create fork'}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
