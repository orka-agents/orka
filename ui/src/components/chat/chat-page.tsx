import { useEffect, useRef } from 'react'
import { ChatMessageList } from './chat-message-list'
import { ChatInput } from './chat-input'
import { useSendMessage, useChatConfig } from '@/hooks/use-chat'
import { useProviderList } from '@/hooks/use-providers'
import { useDocumentTitle } from '@/hooks/use-document-title'
import { useChatStore } from '@/stores/chat'
import { useUIStore } from '@/stores/ui'
import { isForbiddenError } from '@/lib/api-client'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Plus } from 'lucide-react'

const SERVER_DEFAULT = '__default__'

export function ChatPage() {
  useDocumentTitle('Chat')
  const sendMessage = useSendMessage()
  const { data: config } = useChatConfig()
  const { data: providersData, error: providersError } = useProviderList()
  const namespace = useUIStore((s) => s.namespace)
  const composerRef = useRef<HTMLTextAreaElement>(null)
  const {
    currentSessionId,
    newSession,
    messages,
    provider,
    model,
    setProvider,
    setModel,
    selectNamespace,
    activeNamespace,
    isStreaming,
  } = useChatStore()
  // Providers are namespace-scoped, so the persisted picker choice is too.
  useEffect(() => {
    selectNamespace(namespace)
  }, [namespace, selectNamespace])
  const providersForbidden = isForbiddenError(providersError)
  // React Query keeps the last successful data alongside a failed refetch; a
  // 403 must not keep rendering Provider names the current token may not list.
  const providers = providersForbidden ? [] : (providersData?.items ?? [])
  // A persisted pick can outlive its Provider (deleted, or listing now
  // forbidden). Sending that stale name would fail provider resolution on
  // every turn, so fall back to the server default once the list has
  // answered and does not contain it.
  // Gated on the store having switched to the rendered namespace: during a
  // namespace change this render still holds the previous namespace's pick
  // while the list already belongs to the new one, and clearing then would
  // erase the new namespace's valid selection right after selectNamespace
  // restores it.
  const providerGone =
    activeNamespace === namespace &&
    provider !== '' &&
    (providersForbidden || (providersData !== undefined && !providers.some((p) => p.name === provider)))
  useEffect(() => {
    if (!providerGone) return
    setProvider('')
    setModel('')
  }, [providerGone, setProvider, setModel])
  // Without a server default provider an empty pick cannot send at all, so
  // when the namespace lists exactly one ready Provider it is preselected;
  // any other shape leaves the picker empty and disables sending with a hint.
  const readyProviders = providers.filter((p) => p.ready !== false)
  const soleReadyProvider =
    activeNamespace === namespace &&
    provider === '' &&
    config !== undefined &&
    !config.provider &&
    readyProviders.length === 1 &&
    providers.length === 1
      ? readyProviders[0]
      : undefined
  useEffect(() => {
    if (!soleReadyProvider) return
    setProvider(soleReadyProvider.name)
    setModel(soleReadyProvider.defaultModel ?? '')
  }, [soleReadyProvider, setProvider, setModel])
  const hasServerDefault = Boolean(config?.provider)
  const noProviderSelected = !provider && config !== undefined && !hasServerDefault
  const providersErrorMessage = providersError
    ? providersForbidden
      ? hasServerDefault
        ? `Not authorized to list Providers in ${namespace}; only the server default is available.`
        : `Not authorized to list Providers in ${namespace}; an explicit Provider is required to send.`
      : `Providers unavailable: ${providersError instanceof Error ? providersError.message : 'unknown error'}`
    : undefined
  const serverDefaultLabel = config?.provider
    ? `Server default (${config.provider}${config.model ? ` / ${config.model}` : ''})`
    : 'Server default'
  const effectiveModel = model || (provider ? providers.find((p) => p.name === provider)?.defaultModel : config?.model)
  // A model override only reaches the server alongside a provider; without a
  // Provider CRD pick it is pinned to the server's configured default provider,
  // and when there is none to pin to the input is disabled.
  const modelNeedsProvider = !provider && !config?.provider

  // New Chat during a reply also cancels the server-side turn (the orphaned
  // turn sends DELETE /chat/{sessionId}), which deletes the chat and its
  // history like Stop does, so it asks first. An idle chat resets at once.
  const handleNewChat = () => {
    if (useChatStore.getState().isStreaming && !confirm('Stop the current reply and delete this chat?')) return
    newSession()
    // New Chat leaves with the transcript it clears, which would drop focus
    // to <body>; the composer takes it for the next message.
    composerRef.current?.focus()
  }

  const handleProviderChange = (value: string) => {
    const next = value === SERVER_DEFAULT ? '' : value
    setProvider(next)
    // Prefill the model with the provider's default so a plain pick just works;
    // the user can still overwrite it.
    setModel(next ? providers.find((p) => p.name === next)?.defaultModel ?? '' : '')
  }

  return (
    // The negative margins cancel <main>'s padding, so the height adds that
    // padding back: the surface fills the pane exactly, the transcript is the
    // only thing that scrolls, and no empty band is left under the input.
    <div className="-m-4 flex h-[calc(100%+2rem)] flex-col md:-m-6 md:h-[calc(100%+3rem)]">
      {/* Header */}
      <div className="border-b border-border bg-card px-4 py-2">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
            <h1 className="text-sm font-semibold">Chat</h1>
            {/* A badge is a centered flex box, so long text ellipsizes in an
                inner span; centering the overflow would show its middle. */}
            {currentSessionId && (
              <Badge variant="secondary" className="min-w-0 max-w-full justify-start font-mono text-[10px]" title={currentSessionId}>
                <span className="truncate">{currentSessionId}</span>
              </Badge>
            )}
            {effectiveModel && (
              <Badge variant="outline" className="min-w-0 max-w-full justify-start text-[10px]" title={effectiveModel}>
                <span className="truncate">{effectiveModel}</span>
              </Badge>
            )}
          </div>
          {/* On narrow screens the controls take their own full-width row and
              share it, so New Chat stays reachable. */}
          <div className="flex w-full min-w-0 items-center gap-2 sm:w-auto sm:justify-end">
            <Select value={provider || (hasServerDefault ? SERVER_DEFAULT : '')} onValueChange={handleProviderChange} disabled={isStreaming}>
              <SelectTrigger className="h-7 min-w-0 flex-1 text-xs sm:w-auto sm:min-w-40 sm:flex-none" aria-label="Chat provider">
                <span className="min-w-0 truncate"><SelectValue placeholder="Provider" /></span>
              </SelectTrigger>
              <SelectContent>
                {hasServerDefault && <SelectItem value={SERVER_DEFAULT}>{serverDefaultLabel}</SelectItem>}
                {providers.map((p) => (
                  <SelectItem key={p.name} value={p.name}>
                    {p.name}
                    {p.type && ` (${p.type})`}
                    {p.ready === false && ' — not ready'}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Input
              aria-label="Chat model"
              aria-describedby={modelNeedsProvider ? 'chat-model-hint' : undefined}
              value={model}
              onChange={(e) => setModel(e.target.value)}
              placeholder={provider ? providers.find((p) => p.name === provider)?.defaultModel || 'model' : config?.model || 'model (server default)'}
              disabled={isStreaming || modelNeedsProvider}
              className="h-7 min-w-0 flex-1 text-xs sm:w-48 sm:flex-none"
            />
            {messages.length > 0 && (
              <Button variant="ghost" size="sm" onClick={handleNewChat} className="h-7 shrink-0 text-xs">
                <Plus className="mr-1 h-3 w-3" /> New Chat
              </Button>
            )}
          </div>
        </div>
        {(providersErrorMessage || modelNeedsProvider) && (
          <div className="mt-1 flex flex-wrap justify-end gap-x-4 text-[11px]">
            {providersErrorMessage && (
              <p role="alert" className={providersForbidden ? 'text-muted-foreground' : 'text-destructive'}>
                {providersErrorMessage}
              </p>
            )}
            {modelNeedsProvider && (
              <p id="chat-model-hint" className="text-muted-foreground">
                Choose a provider to override the model.
              </p>
            )}
          </div>
        )}
      </div>

      {/* Messages */}
      <ChatMessageList />

      {/* Input */}
      <ChatInput textareaRef={composerRef} onSend={sendMessage} disabled={noProviderSelected} disabledHint={noProviderSelected ? 'Choose a provider before sending.' : undefined} />
    </div>
  )
}
