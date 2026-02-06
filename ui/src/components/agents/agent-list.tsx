import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Bot, Plus } from 'lucide-react'
import { useAgentList } from '@/hooks/use-agents'
import type { Agent } from '@/schemas/agent'

export function AgentList() {
  const { data, isLoading } = useAgentList()

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Agents</h1>
          <p className="text-muted-foreground">Registered AI agent configurations</p>
        </div>
        <Link to="/agents/new">
          <Button><Plus className="mr-2 h-4 w-4" />New Agent</Button>
        </Link>
      </div>

      {isLoading ? (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Card key={i}>
              <CardHeader><Skeleton className="h-5 w-32" /></CardHeader>
              <CardContent><Skeleton className="h-4 w-48" /></CardContent>
            </Card>
          ))}
        </div>
      ) : (data?.items ?? []).length === 0 ? (
        <div className="text-center py-12 text-muted-foreground">No agents registered.</div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {(data?.items ?? []).map((agent: Agent) => (
            <Link key={agent.metadata.uid || agent.metadata.name} to="/agents/$agentId" params={{ agentId: agent.metadata.name }}>
              <Card className="hover:border-primary/50 transition-colors cursor-pointer">
                <CardHeader className="flex flex-row items-center gap-3 space-y-0">
                  <div className="flex h-10 w-10 items-center justify-center rounded-full bg-primary/10">
                    <Bot className="h-5 w-5 text-primary" />
                  </div>
                  <div>
                    <CardTitle className="text-base">{agent.metadata.name}</CardTitle>
                    <p className="text-xs text-muted-foreground">{agent.metadata.namespace}</p>
                  </div>
                </CardHeader>
                <CardContent className="space-y-2">
                  <div className="flex flex-wrap gap-2 text-xs">
                    {agent.spec.model?.provider && <Badge variant="secondary">{agent.spec.model.provider}</Badge>}
                    {agent.spec.model?.name && <Badge variant="outline">{agent.spec.model.name}</Badge>}
                    {agent.spec.runtime && <Badge variant="secondary">{agent.spec.runtime.type} runtime</Badge>}
                  </div>
                  <div className="flex items-center justify-between text-sm text-muted-foreground">
                    <span>Active: {agent.status?.activeTasks ?? 0}</span>
                    {(agent.spec.tools?.length ?? 0) > 0 && <span>{agent.spec.tools!.length} tools</span>}
                  </div>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  )
}
