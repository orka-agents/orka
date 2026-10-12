import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link2, Unplug } from 'lucide-react'
import { toast } from 'sonner'
import { api, isConflictError, isForbiddenError, isNotFoundError, isNotImplementedError, isUnauthorizedError } from '@/lib/api-client'
import {
  callbackReasonMessage, clearConsentCallback, completionCommand, isKubernetesNamespace, openAuthorizeURL, readCompletionToken,
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
  /** Drops the callback from the router's own state once it is spent, so a remount never sees it again. */
  clearCallback?: () => void
}

export function ConnectorsPage({ search, clearCallback }: ConnectorsPageProps) {
  const namespace = useUIStore((s) => s.namespace)
  // A completion spent in another namespace remounts the content before the
  // router has cleared the callback search. The spent callback is remembered
  // here, outside the namespace-keyed content, so the new content never
  // replays it (and never reports a consent "without a completion token").
  const [callbackSpent, setCallbackSpent] = useState(false)
  const spend = () => {
    setCallbackSpent(true)
    clearCallback?.()
  }
  return <ConnectorsPageContent key={namespace} namespace={namespace} search={callbackSpent ? {} : (search ?? {})} clearCallback={spend} />
}

function ConnectorsPageContent({ namespace, search, clearCallback }: { namespace: string; search: ConnectorCallbackSearch; clearCallback?: () => void }) {
  const queryClient = useQueryClient()
  const setNamespace = useUIStore((s) => s.setNamespace)
  const params = namespace ? { namespace } : undefined
  const providers = useQuery({
    queryKey: ['connectors', namespace],
    queryFn: () => api.get<{ items: ConnectorProvider[] }>('/connectors', params),
  })
  // What this page asked the server for and has not yet seen reflected:
  // the list is served through an eventually consistent cache, so after a
  // mode change or a disconnect the page keeps polling until the list
  // shows the requested mode, or no longer shows the link at all.
  const expectations = useRef(new Map<string, (connection: Connection | undefined) => boolean>())
  // Only data the server returned settles an expectation; the optimistic
  // cache writes below never do, or a stale refetch could end the polling.
  const settleExpectations = (items: Connection[]) => {
    for (const [name, satisfied] of expectations.current) {
      if (satisfied(items.find((c) => c.name === name))) expectations.current.delete(name)
    }
  }
  const connections = useQuery({
    queryKey: ['connections', namespace],
    queryFn: async () => {
      const data = await api.get<{ items: Connection[] }>('/connections', params)
      settleExpectations(data.items)
      return data
    },
    // Keep polling while a link is being established or torn down, so a
    // disconnect whose finalizer is still revoking tokens resolves on screen.
    refetchInterval: (query) => {
      const items = query.state.data?.items
      return items?.some((c) => c.state === 'Pending' || c.deleting) || expectations.current.size > 0 ? 5000 : false
    },
  })
  const invalidate = (target: string = namespace) => queryClient.invalidateQueries({ queryKey: ['connections', target] })
  /** Writes the server's own view of a link into the list cache right away. */
  const applyConnection = (connection: Connection) => {
    queryClient.setQueryData<{ items: Connection[] }>(['connections', namespace], (current) => {
      if (!current) return current
      const items = current.items.some((c) => c.name === connection.name)
        ? current.items.map((c) => (c.name === connection.name ? connection : c))
        : [...current.items, connection]
      return { items }
    })
  }

  // Finish a consent the provider just sent us back from. The completion
  // token stays in the fragment until the server accepts it, so a failed
  // attempt can be retried here or by reloading; it is spent in the
  // namespace the consent was sealed in, never the page's current pick.
  const returning = search.status === 'pending' && Boolean(search.connection)
  const [completionToken] = useState(() => (returning ? readCompletionToken() : null))
  // The namespace arrives in the address bar: anything that is not a
  // Kubernetes namespace is ignored rather than used or persisted.
  const callbackNamespace = isKubernetesNamespace(search.namespace) ? search.namespace : undefined
  const completionNamespace = callbackNamespace || namespace
  const completionParams = completionNamespace ? { namespace: completionNamespace } : undefined
  const completionAttempted = useRef(false)
  // A failed consent that was sealed in another namespace is recovered
  // there, but only when the person asks: the error redirect is not
  // authenticated, so its namespace is offered as a switch rather than
  // written into the persisted selection (the content remounts with the
  // same callback search, so the error notice is kept).
  const failedElsewhere = search.status === 'error' && callbackNamespace && callbackNamespace !== namespace ? callbackNamespace : undefined
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
      clearCallback?.()
      const linkedIn = connection.namespace || completionNamespace
      if (linkedIn && linkedIn !== namespace) {
        // The consent was sealed in another namespace: switch the page to
        // it (which remounts this content), or the new link would be
        // invisible behind a success notice. The toast outlives the remount.
        toast.success(`Linked ${connection.provider} (${connection.mode}) in ${linkedIn}`)
        void invalidate(linkedIn)
        setNamespace(linkedIn)
        return
      }
      setCallbackNotice({ tone: 'info', text: `Linked ${connection.provider} (${connection.mode}).` })
      applyConnection(connection)
      void invalidate()
    },
    onError: (error: unknown) => {
      // A conflict that asks for a retry (the controller has not adopted
      // the Connection yet, the provider is not accepted right now, or a
      // committed completion still needs its status pass) keeps the token;
      // the API phrases every retryable conflict with "retry". Any other
      // conflict means the consent expired or was superseded.
      if (isConflictError(error) && !isRetryableConflict(error)) {
        clearConsentCallback()
        clearCallback?.()
        setCallbackNotice({ tone: 'error', text: `This consent can no longer be finished (${errorText(error)}). Start the link again.` })
        return
      }
      if (isForbiddenError(error) && scopeDenied(error)) {
        // Signed in as themselves, but the delegated token may not manage
        // links; the CLI fallback would fail with the same token.
        setCallbackNotice({ tone: 'error', text: `Could not finish linking: ${errorText(error)}. This token lacks the scope your controller requires to manage linked accounts; finish with a token that carries it.`, retry: true })
        return
      }
      // The fallback names the namespace the request actually tried.
      const command = completionCommand(search.connection, completionNamespace)
      const fallback = command ? `, or run: ${command}` : ''
      // The API answers 404 for another person's Connection rather than
      // revealing it, so a dashboard signed in as someone else sees "not
      // found"; it gets the same sign-in and CLI fallback.
      const hint = isForbiddenError(error) || isUnauthorizedError(error)
        ? ` Sign in as yourself and retry${fallback}.`
        : isNotFoundError(error)
          ? ` If you are signed in as someone else, sign in as the person who started this link and retry${fallback}.`
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
        const wanted = result.connection.mode
        applyConnection(result.connection)
        expectations.current.set(result.connection.name, (c) => c?.mode === wanted)
        void invalidate()
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
    onSuccess: (_result, name) => {
      toast.success('Disconnect requested')
      // Shown as disconnecting right away, and polled until it is gone.
      queryClient.setQueryData<{ items: Connection[] }>(['connections', namespace], (current) =>
        current ? { items: current.items.map((c) => (c.name === name ? { ...c, deleting: true, ready: false } : c)) } : current)
      expectations.current.set(name, (c) => c === undefined)
      void invalidate()
    },
    onError: (error: unknown) => toast.error(`Could not disconnect: ${errorText(error)}`),
  })

  const busy = start.isPending || changeMode.isPending || reauthorize.isPending || disconnect.isPending
  // One link per provider card; any further link to the same provider is
  // shown on its own, disconnectable, because tools refuse to run while
  // duplicates exist.
  const byProvider = new Map<string, Connection>()
  const extras: Connection[] = []
  for (const c of connections.data?.items ?? []) {
    if (byProvider.has(c.provider)) extras.push(c)
    else byProvider.set(c.provider, c)
  }
  // A Connection outlives its ConnectorProvider on purpose (it still holds
  // the person's tokens), so it is shown, and can be disconnected, even
  // when no provider card claims it.
  const providerNames = new Set((providers.data?.items ?? []).map((p) => p.name))
  const retained = (connections.data?.items ?? []).filter((c) => !providerNames.has(c.provider))
  const duplicates = extras.filter((c) => providerNames.has(c.provider))
  // Resolution refuses every link to a provider while duplicates exist, so
  // the card's own link is unusable too, not only the extra ones.
  const duplicated = new Set(duplicates.map((c) => c.provider))

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
          {failedElsewhere && (
            <Button size="sm" variant="outline" onClick={() => setNamespace(failedElsewhere)}>Switch to {failedElsewhere}</Button>
          )}
        </div>
      )}
      {providers.isPending || connections.isPending ? (
        <div className="space-y-3" data-testid="connectors-loading">
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
        </div>
      ) : providers.error || connections.error ? (
        isNotImplementedError(providers.error ?? connections.error) ? (
          <EmptyState icon={Unplug} headline="Connectors are disabled on this controller"
            hint="An operator starts the controller with --connectors-enabled (and Task provenance admission) before accounts can be linked." />
        ) : isForbiddenError(providers.error ?? connections.error) ? (
          scopeDenied(providers.error ?? connections.error) ? (
            <EmptyState icon={Link2} headline="This token cannot read linked accounts"
              hint={`You are signed in, but this context token is not delegated the connector scope: ${errorText(providers.error ?? connections.error)}. Use a token that carries the scope your controller requires to read linked accounts.`} />
          ) : (
            <EmptyState icon={Link2} headline="Sign in as yourself to link accounts"
              hint="Linked accounts belong to a person. This token is not a personal identity (OIDC or context token), so there is nothing to link here." />
          )
        ) : (
          <ListAccessError error={providers.error ?? connections.error} resource="connectors" />
        )
      ) : (providers.data?.items.length ?? 0) === 0 && retained.length === 0 ? (
        <EmptyState icon={Unplug} headline="No connector providers"
          hint="An operator adds a ConnectorProvider (for example GitHub) before accounts can be linked." />
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {providers.data!.items.map((provider) => (
            <ProviderCard key={provider.name} provider={provider} connection={byProvider.get(provider.name)} busy={busy}
              duplicate={duplicated.has(provider.name)}
              onConnect={(mode) => start.mutate({ provider: provider.name, mode })}
              onChangeMode={(name, mode) => changeMode.mutate({ name, mode })}
              onReauthorize={(name) => reauthorize.mutate(name)}
              onDisconnect={(name) => disconnect.mutate(name)} />
          ))}
          {retained.map((connection) => (
            <RetainedConnectionCard key={connection.name} connection={connection} busy={busy}
              onDisconnect={(name) => disconnect.mutate(name)} />
          ))}
          {duplicates.map((connection) => (
            <RetainedConnectionCard key={connection.name} connection={connection} busy={busy} duplicate
              onDisconnect={(name) => disconnect.mutate(name)} />
          ))}
        </div>
      )}
    </div>
  )
}

/** A linked account outside its provider card: the provider is gone, or this is an extra link to it. Still yours, still disconnectable. */
function RetainedConnectionCard({ connection, busy, duplicate, onDisconnect }: { connection: Connection; busy: boolean; duplicate?: boolean; onDisconnect: (name: string) => void }) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
        <div>
          <CardTitle className="text-base">{connection.provider} · {connection.name}</CardTitle>
          <p className="text-xs text-muted-foreground">{duplicate ? 'Extra link to this provider' : 'Provider no longer configured'}</p>
        </div>
        <Badge variant="outline">{connection.deleting ? 'Disconnecting…' : connection.state}</Badge>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <p className="text-muted-foreground">
          {duplicate
            ? 'You hold more than one link to this provider; its tools will not run until the extra links are disconnected.'
            : connection.message || 'This provider was removed by an operator. The link keeps its tokens until you disconnect it.'}
        </p>
        {!connection.deleting && (
          <Button size="sm" variant="destructive" disabled={busy} onClick={() => onDisconnect(connection.name)}>Disconnect</Button>
        )}
      </CardContent>
    </Card>
  )
}

interface ProviderCardProps {
  provider: ConnectorProvider
  connection?: Connection
  busy: boolean
  /** The person holds more than one link to this provider, so none of them can be used. */
  duplicate?: boolean
  onConnect: (mode: ConnectionMode) => void
  onChangeMode: (name: string, mode: ConnectionMode) => void
  onReauthorize: (name: string) => void
  onDisconnect: (name: string) => void
}

function ProviderCard({ provider, connection, busy, duplicate, onConnect, onChangeMode, onReauthorize, onDisconnect }: ProviderCardProps) {
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
        <ConnectionBadge provider={provider} connection={connection} duplicate={duplicate} />
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        {readTools.length > 0 && <p><span className="text-muted-foreground">Reads:</span> {readTools.join(', ')}</p>}
        {writeTools.length > 0 && <p><span className="text-muted-foreground">Writes (ask for approval):</span> {writeTools.join(', ')}</p>}
        {connection?.message && !duplicate && (connection.state !== 'Ready' || !connection.ready) && <p className="text-muted-foreground">{connection.message}</p>}
        {connection?.deleting ? (
          <p className="text-muted-foreground">Revoking tokens and removing the link.</p>
        ) : connection && duplicate ? (
          <div className="flex flex-wrap gap-2">
            <p className="w-full text-muted-foreground">You hold more than one link to this provider; its tools will not run until the extra links are disconnected.</p>
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => onDisconnect(connection.name)}>Disconnect</Button>
          </div>
        ) : connection && !provider.ready ? (
          <div className="flex flex-wrap gap-2">
            <p className="w-full text-muted-foreground">The provider is not accepted right now, so this link cannot be used or changed until it is.</p>
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => onDisconnect(connection.name)}>Disconnect</Button>
          </div>
        ) : connection ? (
          <div className="flex flex-wrap gap-2">
            {connection.mode === 'readWrite' ? (
              <Button size="sm" variant="outline" disabled={busy} onClick={() => onChangeMode(connection.name, 'readOnly')}>Limit to reads</Button>
            ) : writeTools.length > 0 ? (
              <Button size="sm" variant="outline" disabled={busy} onClick={() => onChangeMode(connection.name, 'readWrite')}>Allow writes</Button>
            ) : null}
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

function ConnectionBadge({ provider, connection, duplicate }: { provider: ConnectorProvider; connection?: Connection; duplicate?: boolean }) {
  if (!connection) return <Badge variant="outline">{provider.ready ? 'Not linked' : 'Unavailable'}</Badge>
  if (connection.deleting) return <Badge variant="secondary">Disconnecting…</Badge>
  if (duplicate) return <Badge variant="secondary">Duplicate links</Badge>
  // A link is usable only while its provider is accepted; the Connection's
  // own conditions can lag a provider change.
  if (!provider.ready) return <Badge variant="secondary">Linked · provider unavailable</Badge>
  // The API folds the current provider's consent and scope checks into
  // ready, so a stale Ready state is shown as needing a reconnect.
  if (connection.ready) return <Badge>{connection.mode === 'readWrite' ? 'Linked · read and write' : 'Linked · read only'}</Badge>
  if (connection.state === 'Ready') return <Badge variant="secondary">Reconnect needed</Badge>
  return <Badge variant="secondary">{connection.state || 'Pending'}</Badge>
}

/** The API words every conflict a later attempt can clear with "retry". */
function isRetryableConflict(error: unknown): boolean {
  return /retry/i.test(errorText(error))
}

/** A 403 from a verified person whose delegated token lacks the connector scope, as opposed to a non-personal identity. */
function scopeDenied(error: unknown): boolean {
  return /not authorized/i.test(errorText(error))
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
