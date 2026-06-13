import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ArrowLeft } from 'lucide-react'
import { PageHeader } from '@/components/layout/page-header'
import { useTool } from '@/hooks/use-tools'

export function ToolDetail({ toolName }: { toolName: string }) {
  const { data: tool, isLoading } = useTool(toolName)

  if (isLoading) {
    return <div className="space-y-4"><Skeleton className="h-8 w-64" /><Skeleton className="h-64 w-full" /></div>
  }

  if (!tool) {
    return <div className="text-muted-foreground">Tool not found</div>
  }

  // Built-in tools have a simpler shape with a `builtin` field
  const isBuiltin = 'builtin' in tool && (tool as Record<string, unknown>).builtin === true
  const httpConfig = !isBuiltin ? tool.spec?.http : undefined
  const mcpConfig = !isBuiltin ? tool.spec?.mcp : undefined
  const actor = !isBuiltin ? tool.status?.actor : undefined

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-4">
        <Link to="/tools"><Button variant="ghost" size="icon"><ArrowLeft className="h-4 w-4" /></Button></Link>
        <div>
          <PageHeader title={isBuiltin ? (tool as Record<string, unknown>).name as string : tool.metadata?.name ?? toolName} />
          <div className="flex items-center gap-2">
            <Badge variant={isBuiltin ? 'default' : 'secondary'}>{isBuiltin ? 'Built-in' : 'Custom'}</Badge>
            {!isBuiltin && tool.metadata?.namespace && <span className="text-muted-foreground">{tool.metadata.namespace}</span>}
          </div>
        </div>
      </div>

      <Card>
        <CardHeader><CardTitle>Description</CardTitle></CardHeader>
        <CardContent>
          <p className="text-sm">{isBuiltin ? (tool as Record<string, unknown>).description as string : tool.spec?.description}</p>
        </CardContent>
      </Card>

      {!isBuiltin && tool.spec && (
        <>
          {httpConfig && (
            <Card>
              <CardHeader><CardTitle>HTTP Configuration</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                {httpConfig.url && <div><span className="text-muted-foreground">URL:</span> {httpConfig.url}</div>}
                <div><span className="text-muted-foreground">Method:</span> {httpConfig.method ?? 'POST'}</div>
                {httpConfig.timeout && <div><span className="text-muted-foreground">Timeout:</span> {httpConfig.timeout}</div>}
                {httpConfig.authInject && <div><span className="text-muted-foreground">Auth Inject:</span> {httpConfig.authInject}</div>}
                {httpConfig.headers && Object.keys(httpConfig.headers).length > 0 && (
                  <div>
                    <span className="text-muted-foreground">Headers:</span>
                    <pre className="mt-1 rounded-md bg-muted p-2 text-xs">{JSON.stringify(httpConfig.headers, null, 2)}</pre>
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {mcpConfig && (
            <Card>
              <CardHeader><CardTitle>MCP Actor Configuration</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div><span className="text-muted-foreground">Path:</span> {mcpConfig.path ?? '/mcp'}</div>
                {tool.status?.endpoint && <div><span className="text-muted-foreground">Endpoint:</span> {tool.status.endpoint}</div>}
                {mcpConfig.substrateActor?.templateRef && (
                  <div>
                    <span className="text-muted-foreground">Template:</span>{' '}
                    {[mcpConfig.substrateActor.templateRef.namespace, mcpConfig.substrateActor.templateRef.name].filter(Boolean).join('/') || 'default'}
                  </div>
                )}
                {mcpConfig.substrateActor?.poolRef && (
                  <div>
                    <span className="text-muted-foreground">Pool:</span>{' '}
                    {[mcpConfig.substrateActor.poolRef.namespace, mcpConfig.substrateActor.poolRef.name].filter(Boolean).join('/') || 'default'}
                  </div>
                )}
                {actor?.actorID && <div><span className="text-muted-foreground">Actor ID:</span> {actor.actorID}</div>}
                {actor?.routeHost && <div><span className="text-muted-foreground">Route Host:</span> {actor.routeHost}</div>}
                {actor?.poolRef && (
                  <div>
                    <span className="text-muted-foreground">Assigned Pool:</span>{' '}
                    {[actor.poolRef.namespace, actor.poolRef.name].filter(Boolean).join('/') || 'default'}
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {!httpConfig && !mcpConfig && (
            <Card>
              <CardHeader><CardTitle>Configuration</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div>
                  <span className="text-muted-foreground">Backend:</span> Not configured
                </div>
              </CardContent>
            </Card>
          )}

          {tool.spec.parameters && (
            <Card>
              <CardHeader><CardTitle>Parameters (JSON Schema)</CardTitle></CardHeader>
              <CardContent>
                <pre className="max-h-64 overflow-auto rounded-md bg-muted p-4 text-xs">{JSON.stringify(tool.spec.parameters, null, 2)}</pre>
              </CardContent>
            </Card>
          )}

          {tool.status && (
            <Card>
              <CardHeader><CardTitle>Status</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div>
                  <span className="text-muted-foreground">Available:</span>{' '}
                  <Badge className={tool.status.available ? 'bg-status-succeeded-bg text-status-succeeded' : 'bg-status-failed-bg text-status-failed'} variant="secondary">
                    {tool.status.available ? 'Yes' : 'No'}
                  </Badge>
                </div>
                {tool.status.lastCheck && <div><span className="text-muted-foreground">Last Check:</span> {new Date(tool.status.lastCheck).toLocaleString()}</div>}
                {tool.status.error && <div className="text-red-500"><span className="text-muted-foreground">Error:</span> {tool.status.error}</div>}
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  )
}
