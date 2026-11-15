import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ListAccessError } from '@/components/ui/list-access-error'
import { PageHeader } from '@/components/layout/page-header'
import { Bot, Plus } from 'lucide-react'
import { useAgentList } from '@/hooks/use-agents'
import type { Agent } from '@/schemas/agent'
import { agentProviderName, agentReadiness, builtInAgentRuntimeLabel } from '@/lib/agent-runtime'

export function AgentList() {
  const { data, isLoading, error } = useAgentList()
  const agents = error ? [] : (data?.items ?? [])

  return (
    <div className="space-y-4">
      <PageHeader
        title="Agents"
        description="Registered AI agent configurations"
        action={
          <Link to="/agents/new">
            <Button><Plus className="mr-2 h-4 w-4" />New Agent</Button>
          </Link>
        }
      />

      {isLoading ? (
        <div className="grid gap-4 lg:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Card key={i}>
              <CardHeader><Skeleton className="h-5 w-32" /></CardHeader>
              <CardContent><Skeleton className="h-4 w-48" /></CardContent>
            </Card>
          ))}
        </div>
      ) : error ? (
        <Card><CardContent><ListAccessError error={error} resource="agents" /></CardContent></Card>
      ) : agents.length === 0 ? (
        <div className="text-center py-12 text-muted-foreground">No agents registered.</div>
      ) : (
        <div className="grid gap-4 lg:grid-cols-2 xl:grid-cols-3">
          {agents.map((agent: Agent) => {
            const providerName = agentProviderName(agent)
            const readiness = agentReadiness(agent)
            const toolCount = agent.spec.tools?.length ?? 0
            return (
              <Link key={agent.metadata.uid || agent.metadata.name} to="/agents/$agentId" params={{ agentId: agent.metadata.name }}>
                <Card className="hover:border-primary/50 transition-colors cursor-pointer">
                  <CardHeader className="flex flex-row items-center gap-3 space-y-0">
                    <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-primary/10">
                      <Bot className="h-5 w-5 text-primary" />
                    </div>
                    {/* The readiness badge sits under the name so it never squeezes the name column. */}
                    <div className="min-w-0 flex-1 space-y-1">
                      <CardTitle className="truncate text-base leading-snug" title={agent.metadata.name}>{agent.metadata.name}</CardTitle>
                      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                        <p className="min-w-0 truncate text-xs text-muted-foreground" title={agent.metadata.namespace}>{agent.metadata.namespace}</p>
                        {readiness && (
                          <Badge
                            variant="secondary"
                            className={readiness.ready ? 'shrink-0 bg-status-succeeded-bg text-status-succeeded' : 'shrink-0 bg-status-failed-bg text-status-failed'}
                            title={readiness.message}
                          >
                            {readiness.ready ? 'Ready' : 'Not ready'}
                          </Badge>
                        )}
                      </div>
                    </div>
                  </CardHeader>
                  <CardContent className="space-y-2">
                    <div className="flex flex-wrap gap-2 text-xs">
                      {providerName && <Badge variant="secondary">{providerName}</Badge>}
                      {agent.spec.model?.name && <Badge variant="outline">{agent.spec.model.name}</Badge>}
                      {agent.spec.runtime && (
                        <Badge variant="secondary">
                          {'type' in agent.spec.runtime ? builtInAgentRuntimeLabel(agent.spec.runtime.type) : `AgentRuntime ${agent.spec.runtime.runtimeRef.name}`}
                        </Badge>
                      )}
                    </div>
                    <div className="flex items-center justify-between text-sm text-muted-foreground">
                      <span>Active: {agent.status?.activeTasks ?? 0}</span>
                      {toolCount > 0 && <span>{toolCount} {toolCount === 1 ? 'tool' : 'tools'}</span>}
                    </div>
                  </CardContent>
                </Card>
              </Link>
            )
          })}
        </div>
      )}
    </div>
  )
}
