import { Link } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { AccessErrorView, NotFoundView } from '@/components/common/not-found-page'
import {
  useCreatePullRequest,
  useDismissFinding,
  useFinding,
  useGeneratePatch,
  usePatchProposals,
  useReopenFinding,
  useValidateFinding,
} from '@/hooks/use-security'
import { isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { downloadTaskArtifact } from '@/hooks/use-task-artifacts'
import { useUIStore } from '@/stores/ui'
import type { SecurityFinding } from '@/schemas/security'
import { PatchProposalCard } from './patch-proposal-card'

function severityVariant(severity?: string): 'destructive' | 'secondary' | 'outline' {
  if (severity === 'critical' || severity === 'high') return 'destructive'
  if (severity === 'medium') return 'secondary'
  return 'outline'
}

export function FindingDetail({ findingId }: { findingId: string }) {
  const namespace = useUIStore((s) => s.namespace)
  const { data: finding, isLoading, error } = useFinding(findingId)

  if (isLoading) {
    return <Skeleton className="h-96 w-full" />
  }

  const backToSecurity = (
    <Button asChild variant="outline">
      <Link to="/security"><ArrowLeft className="mr-1 h-4 w-4" />Back to Security</Link>
    </Button>
  )

  // A 403 replaces cached data (the token lost access); any other failure
  // keeps what already loaded and only blocks a page that never loaded.
  if (isForbiddenError(error) || (error && !isNotFoundError(error) && !finding)) {
    return <AccessErrorView title="Could not load finding" resource={`finding ${findingId}`} error={error} action={backToSecurity} />
  }

  // A 404 wins over cached data. The patch proposals query lives in
  // FindingDetailContent, so a missing finding sends no further requests.
  if (isNotFoundError(error) || !finding) {
    return (
      <NotFoundView
        title="Finding not found"
        headline={`No finding with ID "${findingId}" in ${namespace}`}
        hint="It may belong to another namespace, or its repository may have been removed."
        action={backToSecurity}
      />
    )
  }

  return <FindingDetailContent findingId={findingId} finding={finding} />
}

function FindingDetailContent({ findingId, finding }: { findingId: string; finding: SecurityFinding }) {
  const namespace = useUIStore((s) => s.namespace)
  const patches = usePatchProposals(findingId)
  const generatePatch = useGeneratePatch(findingId)
  const validateFinding = useValidateFinding(findingId)
  const createPullRequest = useCreatePullRequest(findingId)
  const dismissFinding = useDismissFinding(findingId)
  const reopenFinding = useReopenFinding(findingId)

  const latestSuccessfulPatch = (patches.data?.items ?? []).find((proposal) => proposal.status === 'succeeded' || proposal.status === 'pr_opened')
  let parsedValidation: Record<string, unknown> | null = null
  if (finding.validationJSON) {
    try {
      parsedValidation = JSON.parse(finding.validationJSON) as Record<string, unknown>
    } catch {
      parsedValidation = null
    }
  }
  type Evidence = NonNullable<SecurityFinding['evidence']>[number]
  const validationArtifactRefs = (finding.evidence ?? []).filter((evidence): evidence is Evidence & { taskName: string; name: string } =>
    evidence.kind === 'artifact' &&
    !!evidence.taskName &&
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
                    <ArtifactDownloadLink
                      taskName={evidence.taskName}
                      filename={evidence.name}
                      namespace={namespace}
                      className="font-medium"
                    >
                      {evidence.label || evidence.name || 'Evidence'}
                    </ArtifactDownloadLink>
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
                  {evidence.quote && <div className="mt-2 whitespace-pre-wrap break-words rounded bg-muted p-2 font-mono text-xs">{evidence.quote}</div>}
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
                    <ArtifactDownloadLink
                      key={`${evidence.taskName || 'task'}-${evidence.name || index}`}
                      taskName={evidence.taskName}
                      filename={evidence.name}
                      namespace={namespace}
                      className="block"
                    >
                      {evidence.label || evidence.name}
                    </ArtifactDownloadLink>
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

// Evidence artifacts download through the authenticated fetch path: a plain
// link to the API cannot carry the bearer token, so the server would answer 401.
function ArtifactDownloadLink({ taskName, filename, namespace, className, children }: {
  taskName: string
  filename: string
  namespace: string
  className?: string
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      className={`text-left text-primary hover:underline ${className ?? ''}`}
      onClick={() => {
        downloadTaskArtifact(taskName, filename, namespace).catch(() => toast.error(`Failed to download ${filename}`))
      }}
    >
      {children}
    </button>
  )
}
