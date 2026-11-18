import { useState } from 'react'
import { toast } from 'sonner'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ListAccessError } from '@/components/ui/list-access-error'
import { useThreatModel, useUpdateThreatModel } from '@/hooks/use-security'
import { isConflictError, isNotFoundError } from '@/lib/api-client'

export function ThreatModelEditor({ repositoryName }: { repositoryName: string }) {
  const { data, isLoading, error, refetch } = useThreatModel(repositoryName)
  const updateThreatModel = useUpdateThreatModel(repositoryName)
  const [content, setContent] = useState('')
  const [initialized, setInitialized] = useState(false)
  // The version the text in the editor is based on. Background refetches
  // must not move it, or a save would overwrite changes it never saw. It
  // stays 0 while no model has loaded, and a save sends that 0 too: it asks
  // the server to create the first model only if none exists yet.
  const [baseVersion, setBaseVersion] = useState(0)
  const [conflict, setConflict] = useState(false)
  const [reloading, setReloading] = useState(false)

  if (data !== undefined && !initialized) {
    setContent(data.content)
    setBaseVersion(data.version)
    setInitialized(true)
  }

  const notFound = isNotFoundError(error)
  // Editing needs a known base: a loaded model, a confirmed "none yet" (404),
  // or text already edited against one. Without it (a 500 or 403 before
  // anything loaded) a save could only conflict, so the editor stays locked.
  const baseKnown = data !== undefined || notFound || initialized
  const loadFailed = !baseKnown && error != null

  const save = async () => {
    try {
      const saved = await updateThreatModel.mutateAsync({ content, source: 'edited', expectedVersion: baseVersion })
      if (saved) setBaseVersion(saved.version)
      setConflict(false)
      toast.success('Threat model saved')
    } catch (saveError) {
      if (isConflictError(saveError)) {
        setConflict(true)
        // Learn what changed (for example that a model now exists) without
        // touching the edits; Reload latest is what replaces them.
        void refetch()
        return
      }
      toast.error(`Failed to save threat model: ${saveError instanceof Error ? saveError.message : 'Unknown error'}`)
    }
  }

  const reloadLatest = async () => {
    setReloading(true)
    const result = await refetch()
    setReloading(false)
    if (result.data) {
      setContent(result.data.content)
      setBaseVersion(result.data.version)
      setConflict(false)
    } else {
      toast.error(`Failed to reload threat model: ${result.error instanceof Error ? result.error.message : 'Unknown error'}`)
    }
  }

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <div>
          <CardTitle>Threat Model</CardTitle>
          <p className="text-sm text-muted-foreground">
            Edit the generated threat model to guide future scans and patch prioritization.
          </p>
        </div>
        <Button onClick={save} disabled={updateThreatModel.isPending || !baseKnown}>Save</Button>
      </CardHeader>
      <CardContent>
        {/* Text being edited stays on screen while a refetch runs. */}
        {isLoading && !initialized ? (
          <Skeleton className="h-72 w-full" />
        ) : (
          <div className="space-y-2">
            {loadFailed && (
              <ListAccessError
                error={error}
                resource="the threat model"
                action={<Button type="button" variant="outline" size="sm" onClick={() => void refetch()}>Retry</Button>}
              />
            )}
            {notFound && (
              <div className="rounded-md border border-dashed border-border bg-muted/40 p-3 text-sm text-muted-foreground">
                No threat model has been generated yet. You can save your own notes now or wait for the first scan to produce one.
              </div>
            )}
            {conflict && (
              <div role="alert" className="flex flex-col gap-3 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm sm:flex-row sm:items-center sm:justify-between">
                <p>
                  The threat model changed since you loaded it, so your edits were not saved. They are still in the editor:
                  copy anything you want to keep, then reload the latest version and apply it again.
                </p>
                <Button type="button" variant="outline" size="sm" onClick={reloadLatest} disabled={reloading}>
                  Reload latest
                </Button>
              </div>
            )}
            <textarea
              aria-label="Threat model"
              value={content}
              disabled={!baseKnown}
              onChange={(event) => {
                setContent(event.target.value)
                // Once the user types, a model that loads later must not
                // replace the text; saving it then reports the conflict.
                setInitialized(true)
              }}
              className="min-h-72 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
              placeholder="Document trust boundaries, auth assumptions, key assets, external integrations, and the attack surfaces worth prioritizing."
            />
          </div>
        )}
      </CardContent>
    </Card>
  )
}
