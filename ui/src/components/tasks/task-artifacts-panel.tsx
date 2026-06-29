import { Download, FileText, AlertTriangle } from 'lucide-react'
import { toast } from 'sonner'
import { useTaskArtifacts, downloadTaskArtifact } from '@/hooks/use-task-artifacts'
import { ApiError } from '@/lib/api-client'
import type { ArtifactMetadata } from '@/schemas/artifact'
import { useUIStore } from '@/stores/ui'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Skeleton } from '@/components/ui/skeleton'

// Human-readable bytes: B -> KB -> MB. Keeps one decimal for KB/MB.
function formatSize(size?: number) {
  if (size == null) return null
  if (size < 1024) return `${size} B`
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`
  return `${(size / (1024 * 1024)).toFixed(1)} MB`
}

export function TaskArtifactsPanel({ taskId, taskUid }: { taskId: string; taskUid?: string }) {
  const namespace = useUIStore((s) => s.namespace)
  const { data, isLoading, error } = useTaskArtifacts(taskId, true, taskUid)
  const artifacts = data?.artifacts ?? []
  // 501 = artifact store disabled (a normal empty), anything else is a real
  // failure that must not be silently shown as "No artifacts".
  const isUnsupported = error instanceof ApiError && error.status === 501
  const failed = error && !isUnsupported

  return (
    <Card>
      <CardHeader>
        <CardTitle>Artifacts</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-12 w-full" />
            <Skeleton className="h-12 w-full" />
            <Skeleton className="h-12 w-full" />
          </div>
        ) : failed ? (
          <EmptyState
            icon={AlertTriangle}
            headline="Couldn't load artifacts"
            hint="The artifact list failed to load — this is a backend/permission error, not an empty result. Retry from the runtime controls."
          />
        ) : artifacts.length === 0 ? (
          <EmptyState
            icon={FileText}
            headline="No artifacts"
            hint="Outputs uploaded by this task appear here."
          />
        ) : (
          <ul className="space-y-2">
            {artifacts.map((artifact: ArtifactMetadata) => {
              const size = formatSize(artifact.size)
              return (
                <li
                  key={artifact.filename}
                  className="flex items-center gap-3 rounded-md border px-3 py-2"
                >
                  <FileText className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                  <span className="min-w-0 flex-1 truncate font-mono text-sm">
                    {artifact.filename}
                  </span>
                  {size && (
                    <span className="shrink-0 text-xs text-muted-foreground">{size}</span>
                  )}
                  {artifact.contentType && (
                    <Badge variant="secondary" className="shrink-0">
                      {artifact.contentType}
                    </Badge>
                  )}
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() =>
                      downloadTaskArtifact(taskId, artifact.filename, namespace).catch(() =>
                        toast.error(`Failed to download ${artifact.filename}`),
                      )
                    }
                  >
                    <Download className="size-4" aria-hidden="true" />
                    Download
                  </Button>
                </li>
              )
            })}
          </ul>
        )}
      </CardContent>
    </Card>
  )
}
