import { Activity, Copy, PlugZap, ShieldCheck } from 'lucide-react'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { useChatConfig } from '@/hooks/use-chat'
import { isMemoryStoreUnavailable, useMemoryList } from '@/hooks/use-memory'
import { useCompatModels, useReadyz } from '@/hooks/use-system'
import { useWhoAmI } from '@/hooks/use-whoami'

function CheckBadge({ ok, label }: { ok: boolean | undefined; label: string }) {
  if (ok === undefined) return <Badge variant="secondary">{label}: unknown</Badge>
  return (
    <Badge
      variant="secondary"
      className={ok ? 'bg-status-succeeded-bg text-status-succeeded' : 'bg-status-failed-bg text-status-failed'}
    >
      {label}: {ok ? 'ok' : 'unhealthy'}
    </Badge>
  )
}

function copyText(value: string, label: string) {
  navigator.clipboard.writeText(value).then(
    () => toast.success(`${label} copied`),
    () => toast.error('Copy failed'),
  )
}

export function SystemPage() {
  const { data: readyz, isLoading: readyzLoading } = useReadyz()
  const { data: chatConfig } = useChatConfig()
  const { data: whoami } = useWhoAmI()
  const memoriesProbe = useMemoryList({ limit: 1 }, false)
  const { data: models, isError: modelsError } = useCompatModels()

  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const openaiBase = `${origin}/openai/v1`
  const anthropicBase = `${origin}/anthropic/v1`

  const memoryEnabled = memoriesProbe.error
    ? !isMemoryStoreUnavailable(memoriesProbe.error)
    : memoriesProbe.isSuccess
      ? true
      : undefined

  return (
    <div className="space-y-4">
      <PageHeader
        title="System"
        description="Controller health, enabled capabilities, and endpoints for connecting external clients"
      />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Activity className="h-4 w-4" />
              Health
            </CardTitle>
            <CardDescription>Live readiness as reported by /readyz</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {readyzLoading ? (
              <Skeleton className="h-8 w-48" />
            ) : (
              <div className="flex flex-wrap gap-2">
                <Badge
                  variant="secondary"
                  className={readyz?.status === 'ok'
                    ? 'bg-status-succeeded-bg text-status-succeeded'
                    : 'bg-status-failed-bg text-status-failed'}
                >
                  API: {readyz?.status ?? 'unknown'}
                </Badge>
                {Object.entries(readyz?.checks ?? {}).map(([check, state]) => (
                  <CheckBadge key={check} ok={state === 'ok'} label={check} />
                ))}
              </div>
            )}
            <div className="flex flex-wrap gap-2">
              <CheckBadge ok={chatConfig ? chatConfig.enabled : undefined} label="chat" />
              <CheckBadge ok={memoryEnabled} label="memory store" />
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <ShieldCheck className="h-4 w-4" />
              Identity
            </CardTitle>
            <CardDescription>How the API server sees this session</CardDescription>
          </CardHeader>
          <CardContent className="space-y-1.5 text-sm">
            {!whoami ? (
              <Skeleton className="h-16 w-full" />
            ) : (
              <>
                <p><span className="text-muted-foreground">Auth:</span> {whoami.authType ?? 'unknown'}</p>
                {whoami.username && <p className="break-all font-mono text-xs">{whoami.username}</p>}
                {whoami.namespace && (
                  <p><span className="text-muted-foreground">Token namespace:</span> {whoami.namespace}</p>
                )}
                {whoami.roles?.length ? (
                  <p><span className="text-muted-foreground">Roles:</span> {whoami.roles.join(', ')}</p>
                ) : null}
              </>
            )}
          </CardContent>
        </Card>
      </div>

      {chatConfig && (
        <Card>
          <CardHeader>
            <CardTitle>Chat orchestrator</CardTitle>
            <CardDescription>Limits and tools for the built-in chat loop</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="grid gap-2 text-sm sm:grid-cols-2 lg:grid-cols-4">
              <p><span className="text-muted-foreground">Default:</span> {chatConfig.provider}/{chatConfig.model}</p>
              <p><span className="text-muted-foreground">Max iterations:</span> {chatConfig.maxIterations}</p>
              <p><span className="text-muted-foreground">Max duration:</span> {chatConfig.maxDuration}</p>
              <p><span className="text-muted-foreground">Concurrent chats:</span> {chatConfig.maxConcurrent}</p>
            </div>
            <div className="flex flex-wrap gap-1">
              {(chatConfig.availableTools ?? []).map((tool) => (
                <Badge key={tool} variant="secondary" className="font-mono text-xs">{tool}</Badge>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <PlugZap className="h-4 w-4" />
            Connect external clients
          </CardTitle>
          <CardDescription>
            Point OpenAI- or Anthropic-compatible tools at Orka. Authenticate with the same bearer token used to sign in.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid gap-3 lg:grid-cols-2">
            <div className="space-y-1">
              <p className="text-sm font-medium">OpenAI-compatible</p>
              <div className="flex items-center gap-1">
                <code className="flex-1 truncate rounded-md bg-muted px-2 py-1.5 font-mono text-xs">{openaiBase}</code>
                <Button variant="ghost" size="icon" aria-label="Copy OpenAI base URL" onClick={() => copyText(openaiBase, 'OpenAI base URL')}>
                  <Copy className="h-3.5 w-3.5" />
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">chat/completions + models. Set X-Orka-Tools: disabled to skip server-side tools.</p>
            </div>
            <div className="space-y-1">
              <p className="text-sm font-medium">Anthropic-compatible</p>
              <div className="flex items-center gap-1">
                <code className="flex-1 truncate rounded-md bg-muted px-2 py-1.5 font-mono text-xs">{anthropicBase}</code>
                <Button variant="ghost" size="icon" aria-label="Copy Anthropic base URL" onClick={() => copyText(anthropicBase, 'Anthropic base URL')}>
                  <Copy className="h-3.5 w-3.5" />
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">messages + models, with a server-side agentic tool loop.</p>
            </div>
          </div>
          <div>
            <p className="mb-1.5 text-sm font-medium">Model catalog</p>
            {modelsError ? (
              <p className="text-sm text-muted-foreground">
                Model catalog unavailable — configure a Provider to populate it.
              </p>
            ) : !models ? (
              <Skeleton className="h-10 w-full" />
            ) : models.length === 0 ? (
              <p className="text-sm text-muted-foreground">No models yet. Create a Provider to expose its models here.</p>
            ) : (
              <div className="flex max-h-40 flex-wrap gap-1 overflow-y-auto">
                {models.map((model) => (
                  <Badge key={model.id} variant="secondary" className="font-mono text-xs">{model.id}</Badge>
                ))}
              </div>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
