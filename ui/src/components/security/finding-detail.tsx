import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import {
  useCreatePullRequest,
  useDismissFinding,
  useFinding,
  useGeneratePatch,
  usePatchProposals,
  useReopenFinding,
  useValidateFinding,
} from '@/hooks/use-security'
import { useUIStore } from '@/stores/ui'
import { PatchProposalCard } from './patch-proposal-card'

function severityVariant(severity?: string): 'destructive' | 'secondary' | 'outline' {
  if (severity === 'critical' || severity === 'high') return 'destructive'
  if (severity === 'medium') return 'secondary'
  return 'outline'
}

export function FindingDetail({ findingId }: { findingId: string }) {
  const namespace = useUIStore((s) => s.namespace)
  const { data: finding, isLoading } = useFinding(findingId)
  const patches = usePatchProposals(findingId)
  const generatePatch = useGeneratePatch(findingId)
  const validateFinding = useValidateFinding(findingId)
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
  let parsedValidation: Record<string, unknown> | null = null
  if (finding.validationJSON) {
    try {
      parsedValidation = JSON.parse(finding.validationJSON) as Record<string, unknown>
    } catch {
      parsedValidation = null
    }
  }
  const validationArtifactRefs = (finding.evidence ?? []).filter((evidence) =>
    evidence.kind === 'artifact' &&
    evidence.taskName &&
    (evidence.name === 'security-validation.json' || evidence.name === 'security-validation.txt'),
  )

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-1">
          <div className="flex items-center gap-2">
            <Badge variant={severityVariant(finding.severity)}>{finding.severity}</Badge>
            <Badge variant="outline">{finding.validationStatus}</Badge>
            <Badge variant="outline">{finding.state}</Badge>
          </div>
          <PageHeader title={finding.title} description={finding.summary} />
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
            variant="outline"
            onClick={async () => {
              try {
                await validateFinding.mutateAsync()
                toast.success('Validation task started')
              } catch (error) {
                toast.error(`Failed to start validation: ${error instanceof Error ? error.message : 'Unknown error'}`)
              }
            }}
            disabled={validateFinding.isPending || finding.validationStatus === 'pending'}
          >
            {finding.validationStatus === 'pending' ? 'Validation Running' : 'Validate / Reproduce'}
          </Button>
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
            {finding.sliceID && <div>Slice: <span className="font-mono text-xs">{finding.sliceID}</span></div>}
            {finding.category && <div>Category: <span className="font-medium">{finding.category}</span></div>}
            {finding.triage && <div>Triage: <span className="font-medium">{finding.triage}</span></div>}
            <div>Confidence: <span className="font-medium">{finding.confidence}</span></div>
            <div>Commit: <span className="font-mono text-xs">{finding.commitSHA || '-'}</span></div>
            <div>Location: <span className="font-mono text-xs">{finding.filePath ? `${finding.filePath}${finding.line ? `:${finding.line}` : ''}` : '-'}</span></div>
            {finding.scanTaskName && <div>Scan task: <span className="font-medium">{finding.scanTaskName}</span></div>}
            <div>Validation: <span className="font-medium">{finding.validationStatus}</span></div>
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
            {finding.reproduction && (
              <div>
                <div className="font-medium">Reproduction</div>
                <p className="text-muted-foreground">{finding.reproduction}</p>
              </div>
            )}
            {finding.suggestedRegressionTest && (
              <div>
                <div className="font-medium">Suggested regression test</div>
                <p className="text-muted-foreground">{finding.suggestedRegressionTest}</p>
              </div>
            )}
            {finding.whyTestsDoNotAlreadyCoverThis && (
              <div>
                <div className="font-medium">Why existing tests miss this</div>
                <p className="text-muted-foreground">{finding.whyTestsDoNotAlreadyCoverThis}</p>
              </div>
            )}
            {finding.minimumFixScope && (
              <div>
                <div className="font-medium">Minimum fix scope</div>
                <p className="text-muted-foreground">{finding.minimumFixScope}</p>
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
                  {evidence.kind === 'artifact' && evidence.taskName && evidence.name ? (
                    <a
                      href={`/api/v1/tasks/${evidence.taskName}/artifacts/${evidence.name}?namespace=${encodeURIComponent(namespace)}`}
                      target="_blank"
                      rel="noreferrer"
                      className="font-medium text-primary hover:underline"
                    >
                      {evidence.label || evidence.name || 'Evidence'}
                    </a>
                  ) : (
                    <div className="font-medium">
                      {evidence.path
                        ? `${evidence.path}${evidence.startLine ? `:${evidence.startLine}${evidence.endLine && evidence.endLine !== evidence.startLine ? `-${evidence.endLine}` : ''}` : ''}`
                        : evidence.label || evidence.name || 'Evidence'}
                    </div>
                  )}
                  <div className="text-muted-foreground">{evidence.kind}</div>
                  {evidence.symbol && <div className="text-xs text-muted-foreground">Symbol: {evidence.symbol}</div>}
                  {evidence.taskName && <div className="text-xs text-muted-foreground">Task: {evidence.taskName}</div>}
                  {evidence.quote && <div className="mt-2 rounded bg-muted p-2 font-mono text-xs">{evidence.quote}</div>}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {finding.validationJSON && (
        <Card>
          <CardHeader><CardTitle>Validation</CardTitle></CardHeader>
          <CardContent className="space-y-4">
            {parsedValidation && typeof parsedValidation.summary === 'string' && (
              <div>
                <div className="font-medium">Summary</div>
                <p className="text-sm text-muted-foreground">{parsedValidation.summary}</p>
              </div>
            )}

            {parsedValidation && Array.isArray(parsedValidation.validation_steps) && parsedValidation.validation_steps.length > 0 && (
              <div>
                <div className="font-medium">Validation Steps</div>
                <div className="mt-2 space-y-2 text-sm text-muted-foreground">
                  {parsedValidation.validation_steps.map((step, index) => (
                    <div key={`${String(step)}-${index}`}>{String(step)}</div>
                  ))}
                </div>
              </div>
            )}

            {parsedValidation && typeof parsedValidation.attack_path_analysis === 'string' && (
              <div>
                <div className="font-medium">Attack-Path Analysis</div>
                <p className="mt-2 whitespace-pre-wrap text-sm text-muted-foreground">{parsedValidation.attack_path_analysis}</p>
              </div>
            )}

            {parsedValidation && (Array.isArray(parsedValidation.assumptions) || Array.isArray(parsedValidation.controls) || Array.isArray(parsedValidation.blindspots)) && (
              <div className="grid gap-4 md:grid-cols-3">
                {Array.isArray(parsedValidation.assumptions) && (
                  <div>
                    <div className="font-medium">Assumptions</div>
                    <div className="mt-2 space-y-2 text-sm text-muted-foreground">
                      {parsedValidation.assumptions.map((item, index) => (
                        <div key={`${String(item)}-${index}`}>{String(item)}</div>
                      ))}
                    </div>
                  </div>
                )}
                {Array.isArray(parsedValidation.controls) && (
                  <div>
                    <div className="font-medium">Controls</div>
                    <div className="mt-2 space-y-2 text-sm text-muted-foreground">
                      {parsedValidation.controls.map((item, index) => (
                        <div key={`${String(item)}-${index}`}>{String(item)}</div>
                      ))}
                    </div>
                  </div>
                )}
                {Array.isArray(parsedValidation.blindspots) && (
                  <div>
                    <div className="font-medium">Blindspots</div>
                    <div className="mt-2 space-y-2 text-sm text-muted-foreground">
                      {parsedValidation.blindspots.map((item, index) => (
                        <div key={`${String(item)}-${index}`}>{String(item)}</div>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            )}

            {validationArtifactRefs.length > 0 && (
              <div>
                <div className="font-medium">Validation Artifacts</div>
                <div className="mt-2 space-y-2 text-sm">
                  {validationArtifactRefs.map((evidence, index) => (
                    <a
                      key={`${evidence.taskName || 'task'}-${evidence.name || index}`}
                      href={`/api/v1/tasks/${evidence.taskName}/artifacts/${evidence.name}?namespace=${encodeURIComponent(namespace)}`}
                      target="_blank"
                      rel="noreferrer"
                      className="block text-primary hover:underline"
                    >
                      {evidence.label || evidence.name}
                    </a>
                  ))}
                </div>
              </div>
            )}

            {!parsedValidation && (
              <pre className="whitespace-pre-wrap rounded-md bg-muted p-4 text-xs">{finding.validationJSON}</pre>
            )}
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
