import { Link, useNavigate } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Separator } from '@/components/ui/separator'
import { EmptyState } from '@/components/ui/empty-state'
import { PageHeader } from '@/components/layout/page-header'
import { Activity, ArrowLeft, Bot, SearchX, ShieldCheck, Trash2, type LucideIcon } from 'lucide-react'
import { useAgent, useAgentSystemPrompt, useDeleteAgent } from '@/hooks/use-agents'
import { isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { toast } from 'sonner'
import { agentProviderName, agentReadiness, builtInAgentRuntimeLabel } from '@/lib/agent-runtime'
import type { AgentSpec } from '@/schemas/agent'

const promptClassName = 'max-h-64 overflow-auto rounded-md bg-muted p-4 text-sm whitespace-pre-wrap'

// A ConfigMap prompt names its source, then shows the referenced key when the
// caller may read that ConfigMap.
function SystemPromptCard({ agentName, prompt }: { agentName: string; prompt: NonNullable<AgentSpec['systemPrompt']> }) {
  const ref = prompt.inline ? null : prompt.configMapRef
  const resolved = useAgentSystemPrompt(agentName, ref)
  let body
  if (!ref) {
    body = <pre tabIndex={0} role="region" aria-label="System prompt" className={promptClassName}>{prompt.inline || '-'}</pre>
  } else if (resolved.isPending) {
    body = <Skeleton className="h-24 w-full" />
  } else if (resolved.isError) {
    body = (
      <p className="text-sm text-muted-foreground">
        {isForbiddenError(resolved.error) ? 'You are not allowed to read this ConfigMap.' : `Could not load the prompt: ${resolved.error instanceof Error ? resolved.error.message : String(resolved.error)}`}
      </p>
    )
  } else {
    body = <pre tabIndex={0} role="region" aria-label="System prompt" className={promptClassName}>{resolved.data.content}</pre>
  }
  return (
    <Card className="md:col-span-2">
      <CardHeader><CardTitle>System Prompt</CardTitle></CardHeader>
      <CardContent className="space-y-3">
        {ref && (
          <p className="text-xs text-muted-foreground">
            From ConfigMap <code className="font-mono">{ref.name}</code>, key <code className="font-mono">{ref.key}</code>
          </p>
        )}
        {body}
      </CardContent>
    </Card>
  )
}

// A missing or unreadable Agent keeps the page heading (and so the tab
// title) and a way back to the list.
function AgentUnavailable({ title, icon, headline, hint, alert = false }: {
  title: string
  icon: LucideIcon
  headline: string
  hint: string
  alert?: boolean
}) {
  return (
    <div className="space-y-4">
      <PageHeader title={title} />
      <Card>
        <CardContent>
          <div role={alert ? 'alert' : undefined}>
            <EmptyState
              icon={icon}
              headline={headline}
              hint={hint}
              action={
                <Button asChild variant="outline" size="sm">
                  <Link to="/agents"><ArrowLeft className="h-4 w-4" /> Back to agents</Link>
                </Button>
              }
            />
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

export function AgentDetail({ agentId }: { agentId: string }) {
  const { data: agent, isLoading, error } = useAgent(agentId)
  const deleteAgent = useDeleteAgent()
  const navigate = useNavigate()

  const handleDelete = async () => {
    if (deleteAgent.isPending || !confirm(`Delete agent "${agentId}"?`)) return
    try {
      await deleteAgent.mutateAsync(agentId)
      toast.success('Agent deleted')
      navigate({ to: '/agents' })
    } catch (err) {
      toast.error(`Failed to delete agent: ${err instanceof Error ? err.message : 'Unknown error'}`)
    }
  }

  if (isLoading) {
    return <div className="space-y-4"><Skeleton className="h-8 w-64" /><Skeleton className="h-64 w-full" /></div>
  }

  // A 403 or 404 wins over cached data (React Query keeps the last
  // successful `data` when a refetch fails); any other failure only replaces
  // the page when nothing was loaded.
  const missing = isNotFoundError(error)
  const forbidden = isForbiddenError(error)
  if (forbidden || (error && !missing && !agent)) {
    const message = error instanceof Error ? error.message : ''
    return (
      <AgentUnavailable
        alert
        title="Could not load agent"
        icon={forbidden ? ShieldCheck : Activity}
        headline={forbidden ? 'Not authorized to view agents' : `Agent "${agentId}" failed to load`}
        hint={forbidden
          ? `Your token lacks agents read permission${message ? ` (${message})` : ''}.`
          : message || 'The agent request failed.'}
      />
    )
  }

  if (missing || !agent) {
    return (
      <AgentUnavailable
        title="Agent not found"
        icon={SearchX}
        headline={`No agent named "${agentId}"`}
        hint="It may have been deleted, or it belongs to another namespace."
      />
    )
  }

  const providerName = agentProviderName(agent)
  const readiness = agentReadiness(agent)
  const runtime = agent.spec.runtime
  const coordination = agent.spec.coordination

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-4">
        <Button asChild variant="ghost" size="icon">
          <Link to="/agents" aria-label="Back to agents"><ArrowLeft className="h-4 w-4" /></Link>
        </Button>
        <div className="flex h-10 w-10 items-center justify-center rounded-full bg-primary/10">
          <Bot className="h-5 w-5 text-primary" />
        </div>
        <PageHeader
          className="flex-1"
          title={agent.metadata.name}
          description={agent.metadata.namespace}
          action={
            // aria-disabled, not disabled: disabling the focused button would drop
            // keyboard focus to <body> when the delete fails.
            <Button
              variant="destructive"
              size="sm"
              onClick={handleDelete}
              aria-disabled={deleteAgent.isPending || undefined}
              className="aria-disabled:opacity-50"
            >
              <Trash2 className="mr-2 h-4 w-4" />{deleteAgent.isPending ? 'Deleting...' : 'Delete'}
            </Button>
          }
        />
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        {(agent.spec.model || providerName) && (
          <Card>
            <CardHeader><CardTitle>Model Configuration</CardTitle></CardHeader>
            <CardContent className="space-y-2 text-sm">
              {providerName && (
                <div>
                  <span className="text-muted-foreground">Provider:</span> {providerName}
                  {agent.spec.providerRef && <span className="text-muted-foreground"> (Provider resource)</span>}
                </div>
              )}
              {agent.spec.model?.name && <div><span className="text-muted-foreground">Model:</span> {agent.spec.model.name}</div>}
              {agent.spec.model?.temperature !== undefined && <div><span className="text-muted-foreground">Temperature:</span> {agent.spec.model.temperature}</div>}
              {agent.spec.model?.contextWindow !== undefined && <div><span className="text-muted-foreground">Context Window:</span> {agent.spec.model.contextWindow}</div>}
              {agent.spec.model?.maxTokens !== undefined && <div><span className="text-muted-foreground">Max Tokens:</span> {agent.spec.model.maxTokens}</div>}
            </CardContent>
          </Card>
        )}

        {runtime && (
          <Card>
            <CardHeader><CardTitle>Agent Runtime</CardTitle></CardHeader>
            <CardContent className="space-y-2 text-sm">
              {'type' in runtime ? (
                <>
                  <div><span className="text-muted-foreground">Source:</span> Orka-managed RuntimePool</div>
                  <div><span className="text-muted-foreground">Profile:</span> <Badge variant="secondary">{builtInAgentRuntimeLabel(runtime.type)}</Badge></div>
                  {runtime.contractVersion && <div><span className="text-muted-foreground">Contract Version:</span> <span className="font-mono">{runtime.contractVersion}</span></div>}
                </>
              ) : (
                <>
                  <div><span className="text-muted-foreground">Source:</span> External v2 AgentRuntime</div>
                  <div><span className="text-muted-foreground">Registration:</span> <Badge variant="secondary">{runtime.runtimeRef.name}</Badge></div>
                </>
              )}
              {runtime.defaultMaxTurns !== undefined && <div><span className="text-muted-foreground">Default Max Turns:</span> {runtime.defaultMaxTurns}</div>}
              {runtime.defaultAllowedTools && (
                <div>
                  <span className="text-muted-foreground">Default Allowed Tools:</span>{' '}
                  {runtime.defaultAllowedTools.length === 0 ? (
                    'None'
                  ) : (
                    <div className="mt-1 flex flex-wrap gap-1">
                      {runtime.defaultAllowedTools.map((tool) => <Badge key={tool} variant="outline" className="text-xs">{tool}</Badge>)}
                    </div>
                  )}
                </div>
              )}
            </CardContent>
          </Card>
        )}

        <Card>
          <CardHeader><CardTitle>Status</CardTitle></CardHeader>
          <CardContent className="space-y-2 text-sm">
            {readiness && (
              <div className="flex flex-wrap items-center gap-2">
                <Badge
                  variant="secondary"
                  className={readiness.ready ? 'bg-status-succeeded-bg text-status-succeeded' : 'bg-status-failed-bg text-status-failed'}
                >
                  {readiness.ready ? 'Ready' : 'Not ready'}
                </Badge>
                {readiness.message && <span className="text-muted-foreground">{readiness.message}</span>}
              </div>
            )}
            <div><span className="text-muted-foreground">Active Tasks:</span> {agent.status?.activeTasks ?? 0}</div>
            {agent.status?.lastUsed && <div><span className="text-muted-foreground">Last Used:</span> {new Date(agent.status.lastUsed).toLocaleString()}</div>}
            {(agent.status?.conditions?.length ?? 0) > 0 && (
              <div className="space-y-1 pt-2">
                <Separator />
                <p className="font-medium pt-1">Conditions</p>
                {agent.status!.conditions!.map((c, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Badge variant={c.status === 'True' ? 'default' : 'secondary'} className="text-xs">{c.type}</Badge>
                    <span>{c.status}</span>
                    {c.message && <span className="text-muted-foreground">— {c.message}</span>}
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>

        {(agent.spec.tools?.length ?? 0) > 0 && (
          <Card>
            <CardHeader><CardTitle>Tools</CardTitle></CardHeader>
            <CardContent>
              <div className="flex flex-wrap gap-2">
                {agent.spec.tools!.map(t => (
                  <Badge key={t.name} variant={t.enabled !== false ? 'default' : 'secondary'} className="text-xs">
                    {t.name} {t.enabled === false && '(disabled)'}
                  </Badge>
                ))}
              </div>
            </CardContent>
          </Card>
        )}

        {agent.spec.systemPrompt && <SystemPromptCard agentName={agent.metadata.name} prompt={agent.spec.systemPrompt} />}

        {agent.spec.soul && (
          <Card className="md:col-span-2">
            <CardHeader><CardTitle>Soul</CardTitle></CardHeader>
            <CardContent>
              <pre tabIndex={0} role="region" aria-label="Soul" className="max-h-64 overflow-auto rounded-md bg-muted p-4 text-sm whitespace-pre-wrap">
                {agent.spec.soul.inline || `ConfigMap: ${agent.spec.soul.configMapRef?.name}/${agent.spec.soul.configMapRef?.key}`}
              </pre>
              {agent.spec.soul.digest && <p className="mt-2 break-all text-xs text-muted-foreground">Expected digest: {agent.spec.soul.digest}</p>}
            </CardContent>
          </Card>
        )}

        {coordination && (
          <Card>
            <CardHeader><CardTitle>Coordination</CardTitle></CardHeader>
            <CardContent className="space-y-2 text-sm">
              <div><span className="text-muted-foreground">Enabled:</span> {coordination.enabled ? 'Yes' : 'No'}</div>
              {coordination.maxConcurrentChildren && <div><span className="text-muted-foreground">Max Concurrent:</span> {coordination.maxConcurrentChildren}</div>}
              {coordination.maxDepth && <div><span className="text-muted-foreground">Max Depth:</span> {coordination.maxDepth}</div>}
              <div><span className="text-muted-foreground">Autonomous:</span> {coordination.autonomous ? 'Yes' : 'No'}</div>
              {coordination.autonomous && (
                <div>
                  <span className="text-muted-foreground">Max Iterations:</span>{' '}
                  {coordination.maxIterations ? coordination.maxIterations : 'Unlimited'}
                </div>
              )}
              {(coordination.allowedAgents?.length ?? 0) > 0 && (
                <div>
                  <span className="text-muted-foreground">Allowed Agents:</span>
                  <div className="mt-1 flex flex-wrap gap-1">
                    {coordination.allowedAgents!.map(a => <Badge key={a.name} variant="outline" className="text-xs">{a.name}</Badge>)}
                  </div>
                </div>
              )}
              {(coordination.approvalRequiredTools?.length ?? 0) > 0 && (
                <div>
                  <span className="text-muted-foreground">Approval Required Tools:</span>
                  <div className="mt-1 flex flex-wrap gap-1">
                    {coordination.approvalRequiredTools!.map((tool) => <Badge key={tool} variant="outline" className="text-xs">{tool}</Badge>)}
                  </div>
                </div>
              )}
            </CardContent>
          </Card>
        )}
      </div>
    </div>
  )
}
