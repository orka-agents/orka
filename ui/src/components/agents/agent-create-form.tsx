import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { PageHeader } from '@/components/layout/page-header'
import { useCreateAgent } from '@/hooks/use-agents'
import { useProviderList } from '@/hooks/use-providers'
import { SecretNameField } from '@/components/ui/secret-name-field'
import { useUIStore } from '@/stores/ui'
import { toast } from 'sonner'
import {
  OPENCODE_REVIEWED_RUNTIME_DEFAULTS,
  isOpenCodeModelID,
  type BuiltInAgentRuntimeType,
} from '@/lib/agent-runtime'

const PROVIDER_DOCS_URL = 'https://orka-agents.github.io/orka/docs/reference/configuration#provider'

export function AgentCreateForm() {
  const navigate = useNavigate()
  const createAgent = useCreateAgent()
  const { data: providersData, isLoading: providersLoading, error: providersError } = useProviderList()
  const namespace = useUIStore((s) => s.namespace)
  const providers = providersError ? [] : (providersData?.items ?? [])

  const [name, setName] = useState('')
  const [mode, setMode] = useState<'ai' | 'runtime'>('ai')

  // Native AI agents reference a Provider resource in the Agent's namespace.
  const [provider, setProvider] = useState('')
  const [providerMissing, setProviderMissing] = useState(false)
  const [model, setModel] = useState('')
  const [temperature, setTemperature] = useState('0.7')
  const [contextWindow, setContextWindow] = useState('')
  const [maxTokens, setMaxTokens] = useState('')
  const [secretRef, setSecretRef] = useState('')
  const [systemPrompt, setSystemPrompt] = useState('')
  const [soul, setSoul] = useState('')

  // ACP runtime mode selects either an Orka-managed profile or a registered v2 runtime.
  const [runtimeSource, setRuntimeSource] = useState<'built-in' | 'external'>('built-in')
  const [runtimeType, setRuntimeType] = useState<BuiltInAgentRuntimeType>('claude')
  const [runtimeRef, setRuntimeRef] = useState('')

  const selectedProvider = providers.find((p) => p.name === provider)

  // Model, token, and runtime fields mean different things per mode (the
  // native max-tokens field and OpenCode's output limit share state), so a
  // mode switch starts the mode-specific fields over instead of carrying them.
  // The role and soul apply to native AI and built-in runtimes alike, so they
  // carry over.
  const handleModeChange = (value: string) => {
    setMode(value as 'ai' | 'runtime')
    setProvider('')
    setProviderMissing(false)
    setModel('')
    setTemperature('0.7')
    setContextWindow('')
    setMaxTokens('')
    setRuntimeSource('built-in')
    setRuntimeType('claude')
    setRuntimeRef('')
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()

    const trimmedModel = model.trim()
    if (mode === 'ai') {
      // The pick must still be listed: a namespace switch leaves a stale name.
      if (!selectedProvider) {
        setProviderMissing(true)
        return
      }
      if (temperature) {
        const parsedTemperature = Number(temperature)
        if (!Number.isFinite(parsedTemperature) || parsedTemperature < 0 || parsedTemperature > 2) {
          toast.error('Temperature must be between 0 and 2')
          return
        }
      }
      if (maxTokens) {
        const parsedNativeMaxTokens = Number(maxTokens)
        if (!Number.isInteger(parsedNativeMaxTokens) || parsedNativeMaxTokens <= 0) {
          toast.error('Max tokens must be a positive integer')
          return
        }
      }
    }
    if (
      mode === 'runtime'
      && runtimeSource === 'built-in'
      && runtimeType === 'opencode'
      && !isOpenCodeModelID(trimmedModel)
    ) {
      toast.error('OpenCode requires a literal provider/model ID')
      return
    }
    if (mode === 'runtime' && runtimeSource === 'built-in' && !trimmedModel) {
      toast.error('Model is required for built-in ACP runtimes')
      return
    }
    const parsedContextWindow = Number(contextWindow)
    const parsedMaxTokens = Number(maxTokens)
    if (mode === 'runtime' && runtimeSource === 'built-in' && runtimeType === 'opencode') {
      if (!Number.isInteger(parsedContextWindow) || parsedContextWindow <= 0) {
        toast.error('OpenCode requires a positive context window')
        return
      }
      if (!Number.isInteger(parsedMaxTokens) || parsedMaxTokens <= 0) {
        toast.error('OpenCode requires a positive max output token limit')
        return
      }
      if (parsedContextWindow <= parsedMaxTokens) {
        toast.error('OpenCode context window must exceed max output tokens')
        return
      }
    }
    if (mode === 'runtime' && runtimeSource === 'external' && !runtimeRef.trim()) {
      toast.error('AgentRuntime name is required')
      return
    }

    if (
      mode === 'runtime'
      && runtimeSource === 'built-in'
      && runtimeType === 'copilot'
      && (systemPrompt.includes('@') || soul.includes('@'))
    ) {
      toast.error('Copilot instructions must not contain @ references; inline the referenced text')
      return
    }

    const spec: Record<string, unknown> = {}

    if (mode === 'ai') {
      spec.providerRef = { name: provider }
      // An omitted model name inherits the Provider's defaultModel.
      const modelConfig = {
        ...(trimmedModel ? { name: trimmedModel } : {}),
        ...(temperature ? { temperature: Number(temperature) } : {}),
        ...(maxTokens ? { maxTokens: Number(maxTokens) } : {}),
      }
      if (Object.keys(modelConfig).length > 0) {
        spec.model = modelConfig
      }
      if (secretRef.trim() && secretRef !== '__none__') {
        spec.secretRef = { name: secretRef.trim() }
      }
    } else {
      spec.runtime = runtimeSource === 'built-in'
        ? runtimeType === 'opencode'
          ? {
              type: runtimeType,
              defaultAllowedTools: [...OPENCODE_REVIEWED_RUNTIME_DEFAULTS.defaultAllowedTools],
              defaultAllowBash: OPENCODE_REVIEWED_RUNTIME_DEFAULTS.defaultAllowBash,
            }
          : { type: runtimeType }
        : { runtimeRef: { name: runtimeRef.trim() } }
      if (runtimeSource === 'built-in' && trimmedModel) {
        spec.model = runtimeType === 'opencode'
          ? { name: trimmedModel, contextWindow: parsedContextWindow, maxTokens: parsedMaxTokens }
          : { name: trimmedModel }
      }
    }

    if (mode === 'ai' || runtimeSource === 'built-in') {
      if (new TextEncoder().encode(soul).length > 8192) {
        toast.error('Soul must not exceed 8192 UTF-8 bytes')
        return
      }
      if (soul.trim()) spec.soul = { inline: soul }
      if (systemPrompt) spec.systemPrompt = { inline: systemPrompt }
    }

    try {
      await createAgent.mutateAsync({ name, namespace, spec })
      toast.success('Agent created')
      navigate({ to: '/agents' })
    } catch (err) {
      toast.error(`Failed to create agent: ${err instanceof Error ? err.message : 'Unknown error'}`)
    }
  }

  return (
    <div className="space-y-4">
      <PageHeader title="Create Agent" description="Configure a native AI agent or an ACP runtime profile" />
      <Card>
        <CardHeader>
          <CardTitle>Agent Configuration</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label htmlFor="agent-name" className="text-sm font-medium">Name</label>
                <Input id="agent-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-agent" required />
              </div>
              <div className="space-y-2">
                <label htmlFor="agent-mode" className="text-sm font-medium">Mode</label>
                <Select value={mode} onValueChange={handleModeChange}>
                  <SelectTrigger id="agent-mode"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ai">Native AI (LLM provider)</SelectItem>
                    <SelectItem value="runtime">ACP agent runtime</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            {mode === 'ai' && (
              <div className="space-y-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label htmlFor="agent-provider" className="text-sm font-medium">Provider</label>
                    <Select
                      value={provider}
                      onValueChange={(value) => {
                        setProvider(value)
                        setProviderMissing(false)
                      }}
                      disabled={providers.length === 0}
                    >
                      <SelectTrigger
                        id="agent-provider"
                        aria-invalid={providerMissing || undefined}
                        aria-describedby={providerMissing ? 'agent-provider-error' : undefined}
                      >
                        <SelectValue placeholder={providersLoading ? 'Loading providers...' : 'Select provider'} />
                      </SelectTrigger>
                      <SelectContent>
                        {providers.map((p) => (
                          <SelectItem key={p.name} value={p.name}>
                            {p.name}
                            {p.type && ` (${p.type})`}
                            {p.ready === false && ' — not ready'}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    {providersError ? (
                      <p role="alert" className="text-xs text-destructive">
                        Providers unavailable: {providersError instanceof Error ? providersError.message : 'unknown error'}
                      </p>
                    ) : !providersLoading && providers.length === 0 ? (
                      <p className="text-xs text-muted-foreground">
                        No Providers in {namespace}. Create a Provider resource first;{' '}
                        <a href={PROVIDER_DOCS_URL} target="_blank" rel="noreferrer" className="text-primary underline">see the Provider reference</a>.
                      </p>
                    ) : null}
                    {providerMissing && (
                      <p id="agent-provider-error" role="alert" className="text-xs text-destructive">
                        Select a Provider for native AI agents.
                      </p>
                    )}
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="agent-model" className="text-sm font-medium">Model</label>
                    <Input
                      id="agent-model"
                      value={model}
                      onChange={(e) => setModel(e.target.value)}
                      placeholder={selectedProvider?.defaultModel || 'Model identifier'}
                    />
                    <p className="text-xs text-muted-foreground">Leave empty to use the Provider&apos;s default model.</p>
                  </div>
                </div>
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label htmlFor="agent-temperature" className="text-sm font-medium">Temperature</label>
                    <Input id="agent-temperature" type="number" step="any" min="0" max="2" value={temperature} onChange={(e) => setTemperature(e.target.value)} />
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="agent-max-tokens" className="text-sm font-medium">Max Tokens</label>
                    <Input id="agent-max-tokens" type="number" min="1" step="1" value={maxTokens} onChange={(e) => setMaxTokens(e.target.value)} />
                  </div>
                </div>
                <div className="space-y-2">
                  <label htmlFor="agent-secret-ref" className="text-sm font-medium">Secret Reference</label>
                  <SecretNameField id="agent-secret-ref" value={secretRef} onChange={setSecretRef} placeholder="Select a secret..." />
                  <p className="text-xs text-muted-foreground">Kubernetes Secret containing native AI provider credentials</p>
                </div>
              </div>
            )}

            {mode === 'runtime' && (
              <div className="space-y-4 rounded-md border bg-muted/20 p-4">
                <div className="space-y-2">
                  <label htmlFor="agent-runtime-source" className="text-sm font-medium">Runtime source</label>
                  <Select value={runtimeSource} onValueChange={(value) => setRuntimeSource(value as 'built-in' | 'external')}>
                    <SelectTrigger id="agent-runtime-source"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="built-in">Orka-managed RuntimePool</SelectItem>
                      <SelectItem value="external">External v2 AgentRuntime</SelectItem>
                    </SelectContent>
                  </Select>
                  <p className="text-xs text-muted-foreground">
                    Runtime credentials and publication credentials are never copied into the ACP process tree.
                  </p>
                </div>

                {runtimeSource === 'built-in' ? (
                  <div className="space-y-2">
                    <label htmlFor="agent-runtime-profile" className="text-sm font-medium">Runtime profile</label>
                    <Select value={runtimeType} onValueChange={(value) => setRuntimeType(value as BuiltInAgentRuntimeType)}>
                      <SelectTrigger id="agent-runtime-profile"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="claude">Claude ACP</SelectItem>
                        <SelectItem value="codex">OpenAI Codex ACP</SelectItem>
                        <SelectItem value="copilot">GitHub Copilot ACP</SelectItem>
                        <SelectItem value="opencode">OpenCode ACP</SelectItem>
                      </SelectContent>
                    </Select>
                    {runtimeType === 'opencode' && (
                      <p className="text-xs text-muted-foreground">
                        Reviewed native-tool defaults: Read, Write, Edit, Bash, Glob, and Grep; bash enabled.
                      </p>
                    )}
                  </div>
                ) : (
                  <div className="space-y-2">
                    <label htmlFor="agent-runtime-ref" className="text-sm font-medium">AgentRuntime name</label>
                    <Input
                      id="agent-runtime-ref"
                      value={runtimeRef}
                      onChange={(e) => setRuntimeRef(e.target.value)}
                      placeholder="external-codex"
                      required
                    />
                    <p className="text-xs text-muted-foreground">The registration must pass the orka.harness.v2 conformance profile.</p>
                  </div>
                )}

                {runtimeSource === 'built-in' && (
                  <div className="space-y-2">
                    <label htmlFor="runtime-model" className="text-sm font-medium">Model</label>
                    <Input
                      id="runtime-model"
                      value={model}
                      onChange={(e) => setModel(e.target.value)}
                      placeholder={runtimeType === 'opencode' ? 'provider/model' : 'Enter model identifier'}
                      required
                    />
                    {runtimeType === 'opencode' && (
                      <p className="text-xs text-muted-foreground">OpenCode model IDs use provider/model form.</p>
                    )}
                  </div>
                )}
                {runtimeSource === 'built-in' && runtimeType === 'opencode' && (
                  <div className="grid gap-4 md:grid-cols-2">
                    <div className="space-y-2">
                      <label htmlFor="runtime-context-window" className="text-sm font-medium">Context Window</label>
                      <Input
                        id="runtime-context-window"
                        type="number"
                        min="1"
                        step="1"
                        value={contextWindow}
                        onChange={(e) => setContextWindow(e.target.value)}
                        placeholder="32768"
                      />
                    </div>
                    <div className="space-y-2">
                      <label htmlFor="runtime-max-tokens" className="text-sm font-medium">Max Output Tokens</label>
                      <Input
                        id="runtime-max-tokens"
                        type="number"
                        min="1"
                        step="1"
                        value={maxTokens}
                        onChange={(e) => setMaxTokens(e.target.value)}
                        placeholder="4096"
                      />
                    </div>
                    <p className="text-xs text-muted-foreground md:col-span-2">
                      Use reviewed ceilings for the selected model. Orka pins them into the immutable runtime profile.
                    </p>
                  </div>
                )}
              </div>
            )}

            {(mode === 'ai' || runtimeSource === 'built-in') && (
              <div className="space-y-2">
                <label htmlFor="agent-system-prompt" className="text-sm font-medium">System Prompt</label>
                <textarea
                  id="agent-system-prompt"
                  className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={systemPrompt}
                  onChange={(e) => setSystemPrompt(e.target.value)}
                  placeholder="Optional system prompt..."
                />
              </div>
            )}

            {(mode === 'ai' || runtimeSource === 'built-in') && (
              <div className="space-y-2">
                <label htmlFor="agent-soul" className="text-sm font-medium">Soul (optional)</label>
                <textarea className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm" id="agent-soul" value={soul} onChange={(e) => setSoul(e.target.value)} rows={5} maxLength={8192}
                  placeholder="Persistent persona and communication defaults, separate from role instructions." />
                <p className="text-xs text-muted-foreground">Maximum 8 KiB. New revisions require a new Session. Use YAML for digest-pinned ConfigMap sources.</p>
              </div>
            )}

            <div className="flex gap-2">
              <Button type="submit" disabled={createAgent.isPending}>
                {createAgent.isPending ? 'Creating...' : 'Create Agent'}
              </Button>
              <Button type="button" variant="outline" onClick={() => navigate({ to: '/agents' })}>
                Cancel
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
