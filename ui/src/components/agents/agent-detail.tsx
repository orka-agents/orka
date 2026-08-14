import { useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Separator } from '@/components/ui/separator'
import { ManifestEditor } from '@/components/ui/manifest-editor'
import { PageHeader } from '@/components/layout/page-header'
import { ArrowLeft, Bot, Pencil, Trash2 } from 'lucide-react'
import { useAgent, useDeleteAgent, useUpdateAgent } from '@/hooks/use-agents'
import { toast } from 'sonner'
import { builtInAgentRuntimeLabel } from '@/lib/agent-runtime'

export function AgentDetail({ agentId }: { agentId: string }) {
  const { data: agent, isLoading } = useAgent(agentId)
  const deleteAgent = useDeleteAgent()
  const updateAgent = useUpdateAgent()
  const navigate = useNavigate()
  const [editing, setEditing] = useState(false)

  const handleDelete = async () => {
    if (!confirm(`Delete agent "${agentId}"?`)) return
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

  if (!agent) {
    return <div className="text-muted-foreground">Agent not found</div>
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-4">
        <Link to="/agents"><Button variant="ghost" size="icon"><ArrowLeft className="h-4 w-4" /></Button></Link>
        <div className="flex h-10 w-10 items-center justify-center rounded-full bg-primary/10">
          <Bot className="h-5 w-5 text-primary" />
        </div>
        <PageHeader
          className="flex-1"
          title={agent.metadata.name}
          description={agent.metadata.namespace}
          action={
            <>
              <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
                <Pencil className="mr-2 h-4 w-4" />Edit spec
              </Button>
              <Button variant="destructive" size="sm" onClick={handleDelete} disabled={deleteAgent.isPending}>
                <Trash2 className="mr-2 h-4 w-4" />{deleteAgent.isPending ? 'Deleting...' : 'Delete'}
              </Button>
            </>
          }
        />
      </div>

      <ManifestEditor
        open={editing}
        onOpenChange={setEditing}
        title={`Edit ${agent.metadata.name}`}
        description="Edits the full AgentSpec. runtime.contractVersion is immutable once set."
        initialValue={{ spec: agent.spec }}
        submitLabel="Save changes"
        pending={updateAgent.isPending}
        onSubmit={async (manifest) => {
          const spec = (manifest.spec ?? manifest) as Record<string, unknown>
          await updateAgent.mutateAsync({ name: agent.metadata.name, spec })
          toast.success(`Agent ${agent.metadata.name} updated`)
          setEditing(false)
        }}
      />

      <div className="grid gap-4 md:grid-cols-2">
        {agent.spec.model && (
          <Card>
            <CardHeader><CardTitle>Model Configuration</CardTitle></CardHeader>
            <CardContent className="space-y-2 text-sm">
              {agent.spec.model.provider && <div><span className="text-muted-foreground">Provider:</span> {agent.spec.model.provider}</div>}
              {agent.spec.model.name && <div><span className="text-muted-foreground">Model:</span> {agent.spec.model.name}</div>}
              {agent.spec.model.temperature !== undefined && <div><span className="text-muted-foreground">Temperature:</span> {agent.spec.model.temperature}</div>}
              {agent.spec.model.contextWindow && <div><span className="text-muted-foreground">Context Window:</span> {agent.spec.model.contextWindow}</div>}
              {agent.spec.model.maxTokens && <div><span className="text-muted-foreground">Max Tokens:</span> {agent.spec.model.maxTokens}</div>}
            </CardContent>
          </Card>
        )}

        {agent.spec.runtime && (
          <Card>
            <CardHeader><CardTitle>Agent Runtime</CardTitle></CardHeader>
            <CardContent className="space-y-2 text-sm">
              {'type' in agent.spec.runtime ? (
                <>
                  <div><span className="text-muted-foreground">Source:</span> Orka-managed RuntimePool</div>
                  <div><span className="text-muted-foreground">Profile:</span> <Badge variant="secondary">{builtInAgentRuntimeLabel(agent.spec.runtime.type)}</Badge></div>
                </>
              ) : (
                <>
                  <div><span className="text-muted-foreground">Source:</span> External v2 AgentRuntime</div>
                  <div><span className="text-muted-foreground">Registration:</span> <Badge variant="secondary">{agent.spec.runtime.runtimeRef.name}</Badge></div>
                </>
              )}
            </CardContent>
          </Card>
        )}

        <Card>
          <CardHeader><CardTitle>Status</CardTitle></CardHeader>
          <CardContent className="space-y-2 text-sm">
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

        {agent.spec.systemPrompt && (
          <Card className="md:col-span-2">
            <CardHeader><CardTitle>System Prompt</CardTitle></CardHeader>
            <CardContent>
              <pre className="max-h-64 overflow-auto rounded-md bg-muted p-4 text-sm whitespace-pre-wrap">
                {agent.spec.systemPrompt.inline || (agent.spec.systemPrompt.configMapRef ? `ConfigMap: ${agent.spec.systemPrompt.configMapRef.name}/${agent.spec.systemPrompt.configMapRef.key}` : '-')}
              </pre>
            </CardContent>
          </Card>
        )}

        {agent.spec.coordination && (
          <Card>
            <CardHeader><CardTitle>Coordination</CardTitle></CardHeader>
            <CardContent className="space-y-2 text-sm">
              <div><span className="text-muted-foreground">Enabled:</span> {agent.spec.coordination.enabled ? 'Yes' : 'No'}</div>
              {agent.spec.coordination.maxConcurrentChildren && <div><span className="text-muted-foreground">Max Concurrent:</span> {agent.spec.coordination.maxConcurrentChildren}</div>}
              {agent.spec.coordination.maxDepth && <div><span className="text-muted-foreground">Max Depth:</span> {agent.spec.coordination.maxDepth}</div>}
              {(agent.spec.coordination.allowedAgents?.length ?? 0) > 0 && (
                <div>
                  <span className="text-muted-foreground">Allowed Agents:</span>
                  <div className="mt-1 flex flex-wrap gap-1">
                    {agent.spec.coordination.allowedAgents!.map(a => <Badge key={a.name} variant="outline" className="text-xs">{a.name}</Badge>)}
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
