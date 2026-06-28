import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { GitFork, ArrowRight } from 'lucide-react'

// Fork provenance annotation keys, mirroring internal/labels/labels.go.
const ANNOTATION_FORK_SOURCE_TASK = 'orka.ai/fork-source-task'
const ANNOTATION_FORK_SOURCE_SEQ = 'orka.ai/fork-source-seq'
const ANNOTATION_FORK_CONTEXT_TRUNCATED = 'orka.ai/fork-context-truncated'

export interface ForkProvenanceProps {
  annotations?: Record<string, string>
}

// Shows where a forked task came from, when the fork annotations are present.
// Renders nothing for tasks that were not created via fork.
export function ForkProvenance({ annotations }: ForkProvenanceProps) {
  const sourceTask = annotations?.[ANNOTATION_FORK_SOURCE_TASK]
  if (!sourceTask) return null

  const sourceSeq = annotations?.[ANNOTATION_FORK_SOURCE_SEQ]
  const truncated = annotations?.[ANNOTATION_FORK_CONTEXT_TRUNCATED] === 'true'

  return (
    <Card data-testid="fork-provenance">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <GitFork className="h-4 w-4" /> Fork provenance
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-muted-foreground">Forked from</span>
          <Link
            to="/tasks/$taskId"
            params={{ taskId: sourceTask }}
            className="inline-flex items-center gap-1 font-mono text-primary hover:underline"
          >
            {sourceTask} <ArrowRight className="h-3 w-3" />
          </Link>
          {sourceSeq && (
            <Badge variant="outline" className="font-mono text-[10px]">after #{sourceSeq}</Badge>
          )}
        </div>
        {truncated && (
          <p className="text-xs text-muted-foreground">
            The forked context was truncated to a bounded window of prior events.
          </p>
        )}
      </CardContent>
    </Card>
  )
}
