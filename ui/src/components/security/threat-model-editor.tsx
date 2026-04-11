import { useState } from 'react'
import { toast } from 'sonner'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { useThreatModel, useUpdateThreatModel } from '@/hooks/use-security'
import { ApiError } from '@/lib/api-client'

export function ThreatModelEditor({ repositoryName }: { repositoryName: string }) {
  const { data, isLoading, error } = useThreatModel(repositoryName)
  const updateThreatModel = useUpdateThreatModel(repositoryName)
  const [content, setContent] = useState('')
  const [initialized, setInitialized] = useState(false)

  const currentContent = data?.content
  if (currentContent !== undefined && !initialized) {
    setContent(currentContent)
    setInitialized(true)
  }

  const notFound = error instanceof ApiError && error.status === 404

  const save = async () => {
    try {
      await updateThreatModel.mutateAsync({ content, source: 'edited' })
      toast.success('Threat model saved')
    } catch (saveError) {
      toast.error(`Failed to save threat model: ${saveError instanceof Error ? saveError.message : 'Unknown error'}`)
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
        <Button onClick={save} disabled={updateThreatModel.isPending}>Save</Button>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-72 w-full" />
        ) : (
          <div className="space-y-2">
            {notFound && (
              <div className="rounded-md border border-dashed border-border bg-muted/40 p-3 text-sm text-muted-foreground">
                No threat model has been generated yet. You can save your own notes now or wait for the first scan to produce one.
              </div>
            )}
            <textarea
              value={content}
              onChange={(event) => setContent(event.target.value)}
              className="min-h-72 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring"
              placeholder="Document trust boundaries, auth assumptions, key assets, external integrations, and the attack surfaces worth prioritizing."
            />
          </div>
        )}
      </CardContent>
    </Card>
  )
}
