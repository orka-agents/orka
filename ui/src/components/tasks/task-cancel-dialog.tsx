import { useState } from 'react'
import { toast } from 'sonner'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useCancelTask } from '@/hooks/use-tasks'
import { ApiError } from '@/lib/api-client'

export interface TaskCancelDialogProps {
  taskName: string
  // The UID of the task the person is confirming; sent so a task recreated
  // under the same name is not cancelled instead.
  taskUID?: string
  open: boolean
  onOpenChange: (open: boolean) => void
}

// TaskCancelDialog confirms stopping a running task. Unlike delete, cancel
// keeps everything the task produced.
export function TaskCancelDialog({ taskName, taskUID, open, onOpenChange }: TaskCancelDialogProps) {
  const cancel = useCancelTask()
  const [reason, setReason] = useState('')
  const [error, setError] = useState<string | null>(null)

  function close(next: boolean) {
    if (!next) {
      setReason('')
      setError(null)
      cancel.reset()
    }
    onOpenChange(next)
  }

  async function submit() {
    if (cancel.isPending) return
    setError(null)
    try {
      const result = await cancel.mutateAsync({ id: taskName, uid: taskUID, reason: reason.trim() || undefined })
      toast.success(result.requested ? `Cancellation requested for ${taskName}` : `${taskName} is already cancelling`)
      close(false)
    } catch (err) {
      setError(err instanceof ApiError || err instanceof Error ? err.message : 'Cancel failed.')
    }
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Cancel {taskName}?</DialogTitle>
          <DialogDescription>
            The task stops. Its result, artifacts, plan and events are kept.
          </DialogDescription>
        </DialogHeader>
        <label className="grid gap-1 text-sm">
          <span>Reason (optional)</span>
          <Input
            value={reason}
            maxLength={500}
            onChange={(e) => setReason(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                void submit()
              }
            }}
          />
        </label>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="ghost" onClick={() => close(false)} disabled={cancel.isPending}>
            Keep running
          </Button>
          <Button variant="destructive" onClick={() => void submit()} disabled={cancel.isPending}>
            {cancel.isPending ? 'Cancelling…' : 'Cancel task'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
