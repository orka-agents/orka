import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link2, Unplug } from 'lucide-react'
import { toast } from 'sonner'
import { api, isForbiddenError, isUnauthorizedError } from '@/lib/api-client'
import {
  callbackReasonMessage, clearConsentCallback, completionCommand, openAuthorizeURL, readCompletionToken,
  type Connection, type ConnectionAuthorizeResponse, type ConnectionMode, type ConnectorCallbackSearch, type ConnectorProvider,
} from '@/lib/connectors'
import { useUIStore } from '@/stores/ui'
import { PageHeader } from '@/components/layout/page-header'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'

interface ConnectorsPageProps {
  /** The callback's query parameters, when the browser just came back from a provider. */
  search?: ConnectorCallbackSearch
}

export function ConnectorsPage({ search }: ConnectorsPageProps) {
  const namespace = useUIStore((s) => s.namespace)
  return <ConnectorsPageContent key={namespace} namespace={namespace} search={search ?? {}} />
}

function ConnectorsPageContent({ namespace, search }: { namespace: string; search: ConnectorCallbackSearch }) {
  const queryClient = useQueryClient()
  const params = namespace ? { namespace } : undefined
  const providers = useQuery({
    queryKey: ['connectors', namespace],
    queryFn: () => api.get<{ items: ConnectorProvider[] }>('/connectors', params),
  })
  const connections = useQuery({
    queryKey: ['connections', namespace],
    queryFn: () => api.get<{ items: Connection[] }>('/connections', params),
    refetchInterval: (query) => (query.state.data?.items.some((c) => c.state === 'Pending') ? 5000 : false),
  })
  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['connections', namespace] })

  // Finish a consent the provider just sent us back from. The completion
  // token stays in the fragment until the server accepts it, so a failed
  // attempt can be retried here or by reloading; it is spent in the
  // namespace the consent was sealed in, never the page's current pick.
  const returning = search.status === 'pending' && Boolean(search.connection)
  const [completionToken] = useState(() => (returning ? readCompletionToken() : null))
  const completionNamespace = search.namespace || namespace
  const completionParams = completionNamespace ? { namespace: completionNamespace } : undefined
  const completionAttempted = useRef(false)
  const [callbackNotice, setCallbackNotice] = useState<{ tone: 'error' | 'info'; text: string; retry?: boolean } | null>(() => {
    if (search.status === 'error') return { tone: 'error', text: callbackReasonMessage(search.reason) }
    if (returning && !completionToken) return { tone: 'error', text: 'The consent came back without a completion token. Start the link again.' }
    return null
  })
  const complete = useMutation({
    mutationFn: ({ name, completion }: { name: string; completion: string }) =>
      api.post<Connection>(`/connections/${encodeURIComponent(name)}/complete`, { completion }, completionParams),
    onSuccess: (connection) => {
      clearConsentCallback()
      setCallbackNotice({ tone: 'info', text: `Linked ${connection.provider} (${connection.mode}).` })
      invalidate()
    },
    onError: (error: unknown) => {
      const command = completionCommand(search.connection, search.namespace)
      const hint = isForbiddenError(error) || isUnauthorizedError(error)
        ? ` Sign in as yourself and retry${command ? `, or run: ${command}` : ''}.`
        : ''
      setCallbackNotice({ tone: 'error', text: `Could not finish linking: ${errorText(error)}.${hint}`, retry: true })
    },
  })
  const retryCompletion = () => {
    if (completionToken && search.connection) complete.mutate({ name: search.connection, completion: completionToken })
  }
  useEffect(() => {
    if (completionAttempted.current || !returning || !completionToken || !search.connection) return
    completionAttempted.current = true
    complete.mutate({ name: search.connection, completion: completionToken })
  }, [returning, completionToken, search.connection, complete])

  const start = useMutation({
    mutationFn: ({ provider, mode }: { provider: string; mode: ConnectionMode }) =>
      api.post<ConnectionAuthorizeResponse>('/connections', { provider, mode }, params),
    onSuccess: (result) => {
      if (result.authorizeURL) openAuthorizeURL(result.authorizeURL)
      else {
        toast.success(`${result.connection.provider} is already linked`)
        invalidate()
      }
    },
    onError: (error: unknown) => toast.error(`Could not start linking: ${errorText(error)}`),
  })
  const changeMode = useMutation({
    mutationFn: ({ name, mode }: { name: string; mode: ConnectionMode }) =>
      api.put<ConnectionAuthorizeResponse>(`/connections/${encodeURIComponent(name)}`, { mode }, params),
    onSuccess: (result) => {
      if (result.authorizeURL) openAuthorizeURL(result.authorizeURL)
      else {
        toast.success(`Mode set to ${result.connection.mode}`)
        invalidate()
      }
    },
    onError: (error: unknown) => toast.error(`Could not change the mode: ${errorText(error)}`),
  })
  const reauthorize = useMutation({
    mutationFn: (name: string) => api.post<ConnectionAuthorizeResponse>(`/connections/${encodeURIComponent(name)}/authorize`, undefined, params),
    onSuccess: (result) => { if (result.authorizeURL) openAuthorizeURL(result.authorizeURL) },
    onError: (error: unknown) => toast.error(`Could not restart consent: ${errorText(error)}`),
  })
  const disconnect = useMutation({
    mutationFn: (name: string) => api.delete<void>(`/connections/${encodeURIComponent(name)}`, params),
    onSuccess: () => { toast.success('Disconnected'); invalidate() },
    onError: (error: unknown) => toast.error(`Could not disconnect: ${errorText(error)}`),
  })

  const busy = start.isPending || changeMode.isPending || reauthorize.isPending || disconnect.isPending
  const byProvider = new Map((connections.data?.items ?? []).map((c) => [c.provider, c]))

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Settings"
        title="Connectors"
        description="Link your own accounts so agents you start can act as you. Tokens stay in the controller; agents only ever see the results."
      />
      {callbackNotice && (
        <div role={callbackNotice.tone === 'error' ? 'alert' : 'status'}
          className={callbackNotice.tone === 'error' ? 'flex flex-wrap items-center gap-3 rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm' : 'rounded-lg border bg-card p-3 text-sm'}>
          <span>{callbackNotice.text}</span>
          {callbackNotice.retry && completionToken && (
            <Button size="sm" variant="outline" disabled={complete.isPending} onClick={retryCompletion}>Retry</Button>
          )}
        </div>
      )}
      {providers.isPending || connections.isPending ? (
        <div className="space-y-3" data-testid="connectors-loading">
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
        </div>
      ) : providers.error || connections.error ? (
        isForbiddenError(providers.error ?? connections.error) ? (
          <EmptyState icon={Link2} headline="Sign in as yourself to link accounts"
            hint="Linked accounts belong to a person. This token is not a personal identity (OIDC or context token), so there is nothing to link here." />
        ) : (
          <ListAccessError error={providers.error ?? connections.error} resource="connectors" />
        )
      ) : (providers.data?.items.length ?? 0) === 0 ? (
        <EmptyState icon={Unplug} headline="No connector providers"
          hint="An operator adds a ConnectorProvider (for example GitHub) before accounts can be linked." />
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {providers.data!.items.map((provider) => (
            <ProviderCard key={provider.name} provider={provider} connection={byProvider.get(provider.name)} busy={busy}
              onConnect={(mode) => start.mutate({ provider: provider.name, mode })}
              onChangeMode={(name, mode) => changeMode.mutate({ name, mode })}
              onReauthorize={(name) => reauthorize.mutate(name)}
              onDisconnect={(name) => disconnect.mutate(name)} />
          ))}
        </div>
      )}
    </div>
  )
}

interface ProviderCardProps {
  provider: ConnectorProvider
  connection?: Connection
  busy: boolean
  onConnect: (mode: ConnectionMode) => void
  onChangeMode: (name: string, mode: ConnectionMode) => void
  onReauthorize: (name: string) => void
  onDisconnect: (name: string) => void
}

function ProviderCard({ provider, connection, busy, onConnect, onChangeMode, onReauthorize, onDisconnect }: ProviderCardProps) {
  const title = provider.displayName || provider.name
  const readTools = provider.tools.filter((t) => t.class === 'read').map((t) => t.name)
  const writeTools = provider.tools.filter((t) => t.class === 'write').map((t) => t.name)
  return (
    <Card data-testid={`connector-${provider.name}`}>
      <CardHeader className="flex flex-row items-start justify-between gap-2 space-y-0">
        <div>
          <CardTitle className="text-base">{title}</CardTitle>
          <p className="text-xs text-muted-foreground">{provider.name}</p>
        </div>
        <ConnectionBadge provider={provider} connection={connection} />
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        {readTools.length > 0 && <p><span className="text-muted-foreground">Reads:</span> {readTools.join(', ')}</p>}
        {writeTools.length > 0 && <p><span className="text-muted-foreground">Writes (ask for approval):</span> {writeTools.join(', ')}</p>}
        {connection?.message && connection.state !== 'Ready' && <p className="text-muted-foreground">{connection.message}</p>}
        {connection ? (
          <div className="flex flex-wrap gap-2">
            {connection.mode === 'readWrite' ? (
              <Button size="sm" variant="outline" disabled={busy} onClick={() => onChangeMode(connection.name, 'readOnly')}>Limit to reads</Button>
            ) : (
              <Button size="sm" variant="outline" disabled={busy} onClick={() => onChangeMode(connection.name, 'readWrite')}>Allow writes</Button>
            )}
            {!connection.ready && (
              <Button size="sm" variant="outline" disabled={busy} onClick={() => onReauthorize(connection.name)}>Reconnect</Button>
            )}
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => onDisconnect(connection.name)}>Disconnect</Button>
          </div>
        ) : (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" disabled={busy || !provider.ready} onClick={() => onConnect('readOnly')}>Connect (read only)</Button>
            {writeTools.length > 0 && (
              <Button size="sm" variant="outline" disabled={busy || !provider.ready} onClick={() => onConnect('readWrite')}>Connect with writes</Button>
            )}
            {!provider.ready && <p className="w-full text-xs text-muted-foreground">This provider is not ready yet; ask an operator.</p>}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function ConnectionBadge({ provider, connection }: { provider: ConnectorProvider; connection?: Connection }) {
  if (!connection) return <Badge variant="outline">{provider.ready ? 'Not linked' : 'Unavailable'}</Badge>
  if (connection.ready) return <Badge>{connection.mode === 'readWrite' ? 'Linked · read and write' : 'Linked · read only'}</Badge>
  return <Badge variant="secondary">{connection.state || 'Pending'}</Badge>
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
