import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState } from '@/components/ui/empty-state'
import { Activity, ArrowLeft, SearchX, ShieldCheck, type LucideIcon } from 'lucide-react'
import { PageHeader } from '@/components/layout/page-header'
import { useTool } from '@/hooks/use-tools'
import { isForbiddenError, isNotFoundError } from '@/lib/api-client'

// A missing or unreadable tool keeps the page heading (and so the tab title)
// and a way back to the list.
function ToolUnavailable({ title, icon, headline, hint, alert = false }: {
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
                  <Link to="/tools"><ArrowLeft className="h-4 w-4" /> Back to tools</Link>
                </Button>
              }
            />
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

export function ToolDetail({ toolName }: { toolName: string }) {
  const { data: tool, isLoading, error } = useTool(toolName)

  if (isLoading) {
    return <div className="space-y-4"><Skeleton className="h-8 w-64" /><Skeleton className="h-64 w-full" /></div>
  }

  // A 403 or 404 wins over cached data (React Query keeps the last
  // successful `data` when a refetch fails); any other failure only replaces
  // the page when nothing was loaded.
  const missing = isNotFoundError(error)
  const forbidden = isForbiddenError(error)
  if (forbidden || (error && !missing && !tool)) {
    const message = error instanceof Error ? error.message : ''
    return (
      <ToolUnavailable
        alert
        title="Could not load tool"
        icon={forbidden ? ShieldCheck : Activity}
        headline={forbidden ? 'Not authorized to view tools' : `Tool "${toolName}" failed to load`}
        hint={forbidden
          ? `Your token lacks tools read permission${message ? ` (${message})` : ''}.`
          : message || 'The tool request failed.'}
      />
    )
  }

  if (missing || !tool) {
    return (
      <ToolUnavailable
        title="Tool not found"
        icon={SearchX}
        headline={`No tool named "${toolName}"`}
        hint="It may have been deleted, or it belongs to another namespace."
      />
    )
  }

  // Built-in tools have a simpler shape with a `builtin` field
  const isBuiltin = 'builtin' in tool && (tool as Record<string, unknown>).builtin === true
  const httpConfig = !isBuiltin ? tool.spec?.http : undefined
  const mcpConfig = !isBuiltin ? tool.spec?.mcp : undefined
  const actor = !isBuiltin ? tool.status?.actor : undefined
  // Built-in tools return a flat {name, builtin, description, parameters}
  // payload; custom tools carry the schema under spec.parameters.
  const rawParameters = isBuiltin ? (tool as Record<string, unknown>).parameters : tool.spec?.parameters
  const parameters =
    rawParameters && typeof rawParameters === 'object' && Object.keys(rawParameters as object).length > 0
      ? rawParameters
      : undefined

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-4">
        <Button asChild variant="ghost" size="icon">
          <Link to="/tools" aria-label="Back to tools"><ArrowLeft className="h-4 w-4" /></Link>
        </Button>
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
                    <pre tabIndex={0} role="region" aria-label="HTTP headers" className="mt-1 max-h-64 overflow-auto rounded-md bg-muted p-2 text-xs">{JSON.stringify(httpConfig.headers, null, 2)}</pre>
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

      {parameters && (
        <Card>
          <CardHeader><CardTitle>Parameters (JSON Schema)</CardTitle></CardHeader>
          <CardContent>
            {/* A named, focusable region, so keyboard users can scroll a schema
                taller or wider than the box. */}
            <pre
              tabIndex={0}
              role="region"
              aria-label="Parameters JSON Schema"
              className="max-h-64 overflow-auto rounded-md bg-muted p-4 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              {JSON.stringify(parameters, null, 2)}
            </pre>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
