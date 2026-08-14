import { useState } from 'react'
import { FileDiff } from 'lucide-react'

import { Button } from '@/components/ui/button'
import {
  Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Skeleton } from '@/components/ui/skeleton'
import { ApiError } from '@/lib/api-client'
import { useImplementationJobPatchPreview } from '@/hooks/use-monitors'
import type { MonitorImplementationJob } from '@/schemas/monitor'

// Read-only view of the validated orka.patch.v1 artifact an implementation
// job produced. 501 = no artifact store; 404 = no patch artifact yet.
export function ImplementationJobPatchDialog({ job }: { job: MonitorImplementationJob }) {
  const [open, setOpen] = useState(false)
  const preview = useImplementationJobPatchPreview(job.id, open)

  const errorText = preview.error instanceof ApiError
    ? preview.error.status === 501
      ? 'The controller has no artifact store configured, so patch artifacts cannot be previewed.'
      : preview.error.status === 404
        ? 'This job has not produced a patch artifact yet.'
        : preview.error.message
    : preview.error instanceof Error
      ? preview.error.message
      : null

  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => setOpen(true)}>
        <FileDiff className="mr-1.5 h-3.5 w-3.5" />
        Preview patch
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="sm:max-w-3xl">
          <DialogHeader>
            <DialogTitle>Patch preview — job {job.id}</DialogTitle>
            <DialogDescription>
              Validated patch artifact for issue #{job.issueNumber}. Publication still goes through the clean-room publisher.
            </DialogDescription>
          </DialogHeader>
          {preview.isLoading && <Skeleton className="h-48 w-full" />}
          {errorText && <p className="text-sm text-muted-foreground">{errorText}</p>}
          {preview.data && (
            <ScrollArea className="max-h-[28rem]">
              <pre className="whitespace-pre-wrap break-words rounded-md bg-muted p-3 font-mono text-xs leading-5">
                {typeof preview.data.patch === 'string'
                  ? preview.data.patch
                  : JSON.stringify(preview.data.patch, null, 2)}
              </pre>
            </ScrollArea>
          )}
        </DialogContent>
      </Dialog>
    </>
  )
}
