import { useMemo } from 'react'
import { useTaskResult } from '@/hooks/use-tasks'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { GitBranch } from 'lucide-react'
import { DiffViewer } from './diff-viewer'
import { TaskFilesChanged } from './task-files-changed'

interface StructuredResult {
  summary?: string
  diff?: string
  verdict?: string
  feedback?: string
  files?: string[]
  pushBranch?: string
}

const verdictStyles: Record<string, string> = {
  APPROVE: 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200',
  REQUEST_CHANGES: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
  COMMENT: 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-200',
}

function tryParseStructuredResult(result: string): StructuredResult | null {
  try {
    const parsed = JSON.parse(result)
    if (typeof parsed === 'object' && parsed !== null && (parsed.summary || parsed.diff || parsed.verdict || parsed.feedback || parsed.files)) {
      return parsed as StructuredResult
    }
    return null
  } catch {
    return null
  }
}

function StructuredResultView({ result }: { result: StructuredResult }) {
  return (
    <div className="space-y-4">
      {result.verdict && (
        <div>
          <Badge className={verdictStyles[result.verdict] ?? ''} variant="secondary" data-testid="verdict-badge">
            {result.verdict}
          </Badge>
        </div>
      )}
      {result.summary && (
        <div>
          <h4 className="mb-1 text-sm font-semibold">Summary</h4>
          <pre className="whitespace-pre-wrap rounded-md bg-muted p-3 text-sm">{result.summary}</pre>
        </div>
      )}
      {result.feedback && (
        <div>
          <h4 className="mb-1 text-sm font-semibold">Feedback</h4>
          <pre className="whitespace-pre-wrap rounded-md bg-muted p-3 text-sm">{result.feedback}</pre>
        </div>
      )}
      {result.files && result.files.length > 0 && (
        <div>
          <h4 className="mb-1 text-sm font-semibold">Files Changed</h4>
          <TaskFilesChanged files={result.files} />
        </div>
      )}
      {result.diff && (
        <div>
          <h4 className="mb-1 text-sm font-semibold">Diff</h4>
          <DiffViewer diff={result.diff} />
        </div>
      )}
      {result.pushBranch && (
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <GitBranch className="size-4" />
          <span>Pushed to branch: <code className="rounded bg-muted px-1 font-mono">{result.pushBranch}</code></span>
        </div>
      )}
    </div>
  )
}

export function TaskResultViewer({ taskId }: { taskId: string }) {
  const { data, isLoading, refetch, isFetched } = useTaskResult(taskId)

  const structured = useMemo(() => {
    if (!data?.result) return null
    return tryParseStructuredResult(data.result)
  }, [data?.result])

  if (!isFetched) {
    return (
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle>Result</CardTitle>
          <Button variant="outline" size="sm" onClick={() => refetch()}>Load Result</Button>
        </CardHeader>
      </Card>
    )
  }

  if (isLoading) {
    return (
      <Card>
        <CardHeader><CardTitle>Result</CardTitle></CardHeader>
        <CardContent><Skeleton className="h-32 w-full" /></CardContent>
      </Card>
    )
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Result</CardTitle>
      </CardHeader>
      <CardContent>
        {structured ? (
          <StructuredResultView result={structured} />
        ) : (
          <pre className="max-h-96 overflow-auto rounded-md bg-muted p-4 text-sm whitespace-pre-wrap">
            {data?.result ?? 'No result available'}
          </pre>
        )}
      </CardContent>
    </Card>
  )
}
