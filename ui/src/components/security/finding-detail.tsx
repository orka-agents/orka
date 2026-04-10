import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import {
  useCreatePullRequest,
  useDismissFinding,
  useFinding,
  useGeneratePatch,
  usePatchProposals,
  useReopenFinding,
} from '@/hooks/use-security'
import { PatchProposalCard } from './patch-proposal-card'

function severityVariant(severity?: string): 'destructive' | 'secondary' | 'outline' {
  if (severity === 'critical' || severity === 'high') return 'destructive'
  if (severity === 'medium') return 'secondary'
  return 'outline'
}

export function FindingDetail({ findingId }: { findingId: string }) {
  const { data: finding, isLoading } = useFinding(findingId)
  const patches = usePatchProposals(findingId)
  const generatePatch = useGeneratePatch(findingId)
  const createPullRequest = useCreatePullRequest(findingId)
  const dismissFinding = useDismissFinding(findingId)
  const reopenFinding = useReopenFinding(findingId)

  if (isLoading) {
    return <Skeleton className="h-96 w-full" />
  }

  if (!finding) {
    return <div className="text-muted-foreground">Finding not found.</div>
  }

  const latestSuccessfulPatch = (patches.data?.items ?? []).find((proposal) => proposal.status === 'succeeded' || proposal.status === 'pr_opened')

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-1">
          <div className="flex items-center gap-2">
            <Badge variant={severityVariant(finding.severity)}>{finding.severity}</Badge>
            <Badge variant="outline">{finding.validationStatus}</Badge>
            <Badge variant="outline">{finding.state}</Badge>
          </div>
          <h1 className="text-3xl font-bold tracking-tight">{finding.title}</h1>
          <p className="text-muted-foreground">{finding.summary}</p>
        </div>
        <div className="flex flex-wrap gap-2">
          {finding.state === 'dismissed' ? (
            <Button
              variant="outline"
              onClick={async () => {
                try {
                  await reopenFinding.mutateAsync()
                  toast.success('Finding reopened')
                } catch (error) {
                  toast.error(`Failed to reopen finding: ${error instanceof Error ? error.message : 'Unknown error'}`)
                }
              }}
            >
              Reopen
            </Button>
          ) : (
            <Button
              variant="outline"
              onClick={async () => {
                try {
                  await dismissFinding.mutateAsync()
                  toast.success('Finding dismissed')
                } catch (error) {
                  toast.error(`Failed to dismiss finding: ${error instanceof Error ? error.message : 'Unknown error'}`)
                }
              }}
            >
              Dismiss
            </Button>
          )}
          <Button
            onClick={async () => {
              try {
                await generatePatch.mutateAsync()
                toast.success('Patch generation started')
              } catch (error) {
                toast.error(`Failed to generate patch: ${error instanceof Error ? error.message : 'Unknown error'}`)
              }
            }}
            disabled={generatePatch.isPending}
          >
            Generate Patch
          </Button>
          <Button
            variant="secondary"
            onClick={async () => {
              try {
                const result = await createPullRequest.mutateAsync()
                toast.success(`Pull request created${result.prNumber ? ` (#${result.prNumber})` : ''}`)
              } catch (error) {
                toast.error(`Failed to open pull request: ${error instanceof Error ? error.message : 'Unknown error'}`)
              }
            }}
            disabled={!latestSuccessfulPatch || createPullRequest.isPending}
          >
            Open PR
          </Button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <Card>
          <CardHeader><CardTitle>Context</CardTitle></CardHeader>
          <CardContent className="space-y-2 text-sm">
            <div>Repository: <span className="font-medium">{finding.repositoryScan}</span></div>
            <div>Confidence: <span className="font-medium">{finding.confidence}</span></div>
            <div>Commit: <span className="font-mono text-xs">{finding.commitSHA || '-'}</span></div>
            <div>Location: <span className="font-mono text-xs">{finding.filePath ? `${finding.filePath}${finding.line ? `:${finding.line}` : ''}` : '-'}</span></div>
            {finding.scanTaskName && <div>Scan task: <span className="font-medium">{finding.scanTaskName}</span></div>}
            {finding.prURL && (
              <a href={finding.prURL} target="_blank" rel="noreferrer" className="text-primary hover:underline">
                Open PR #{finding.prNumber}
              </a>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader><CardTitle>Remediation</CardTitle></CardHeader>
          <CardContent className="space-y-3 text-sm">
            {finding.rootCause && (
              <div>
                <div className="font-medium">Root cause</div>
                <p className="text-muted-foreground">{finding.rootCause}</p>
              </div>
            )}
            {finding.remediation && (
              <div>
                <div className="font-medium">Guidance</div>
                <p className="text-muted-foreground">{finding.remediation}</p>
              </div>
            )}
            {finding.suggestedAction && (
              <div>
                <div className="font-medium">Suggested action</div>
                <p className="text-muted-foreground">{finding.suggestedAction}</p>
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader><CardTitle>Evidence</CardTitle></CardHeader>
        <CardContent>
          {(finding.evidence ?? []).length === 0 ? (
            <div className="text-sm text-muted-foreground">No evidence artifacts referenced.</div>
          ) : (
            <div className="space-y-2">
              {(finding.evidence ?? []).map((evidence, index) => (
                <div key={`${evidence.name || evidence.label || 'evidence'}-${index}`} className="rounded-md border border-border p-3 text-sm">
                  <div className="font-medium">{evidence.label || evidence.name || 'Evidence'}</div>
                  <div className="text-muted-foreground">{evidence.kind}</div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {finding.validationJSON && (
        <Card>
          <CardHeader><CardTitle>Validation Output</CardTitle></CardHeader>
          <CardContent>
            <pre className="whitespace-pre-wrap rounded-md bg-muted p-4 text-xs">{finding.validationJSON}</pre>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader><CardTitle>Patch Proposals</CardTitle></CardHeader>
        <CardContent>
          {patches.isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : (patches.data?.items ?? []).length === 0 ? (
            <div className="text-sm text-muted-foreground">No patch proposals yet.</div>
          ) : (
            <div className="space-y-3">
              {(patches.data?.items ?? []).map((proposal) => (
                <PatchProposalCard key={proposal.id} proposal={proposal} />
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
