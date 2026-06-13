import { useState } from 'react'
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

export function ForkDialog({ taskId, event, open, onOpenChange }: ForkDialogProps) {
  const fork = useForkTask(taskId)
  const [newTaskName, setNewTaskName] = useState('')
  const [agentName, setAgentName] = useState('')
  const [prompt, setPrompt] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [created, setCreated] = useState<ForkTaskResponse | null>(null)

  const afterSeq = event?.seq ?? 0

  function reset() {
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
    try {
      const result = await fork.mutateAsync({
        afterSeq,
        newTaskName: newTaskName.trim() || undefined,
        agentRef: agentName.trim() ? { name: agentName.trim() } : undefined,
        prompt: prompt.trim() || undefined,
      })
      setCreated(result)
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.message || `Fork failed (${err.status}).`)
      } else {
        setError(err instanceof Error ? err.message : 'Fork failed.')
      }
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) reset()
        onOpenChange(next)
      }}
    >
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
              onClick={() => onOpenChange(false)}
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
                onChange={(e) => setNewTaskName(e.target.value)}
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
                onChange={(e) => setAgentName(e.target.value)}
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
                onChange={(e) => setPrompt(e.target.value)}
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
            <Button variant="outline" onClick={() => onOpenChange(false)}>Close</Button>
          ) : (
            <>
              <Button variant="outline" onClick={() => onOpenChange(false)} disabled={fork.isPending}>
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
