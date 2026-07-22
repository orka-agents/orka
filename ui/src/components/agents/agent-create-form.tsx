import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { PageHeader } from '@/components/layout/page-header'
import { useCreateAgent } from '@/hooks/use-agents'
import { useSecretNames } from '@/hooks/use-secrets'
import { useUIStore } from '@/stores/ui'
import { toast } from 'sonner'

const defaultRuntimeAllowedTools = 'Read,Glob,Grep,Bash,LS'
const defaultOpencodeAllowedTools = 'Read,Glob,LS'
const defaultOpencodeMaxTokens = 8192
const defaultOpencodeContextWindow = 128000
const maxOpencodeOutputTokens = 32000

export function AgentCreateForm() {
  const navigate = useNavigate()
  const createAgent = useCreateAgent()
  const { data: secretsData } = useSecretNames()
  const namespace = useUIStore((s) => s.namespace)

  const [name, setName] = useState('')
  const [mode, setMode] = useState<'ai' | 'runtime'>('ai')

  // AI mode fields
  const [provider, setProvider] = useState('')
  const [model, setModel] = useState('')
  const [temperature, setTemperature] = useState('0.7')
  const [maxTokens, setMaxTokens] = useState('')
  const [contextWindow, setContextWindow] = useState('')
  const [secretRef, setSecretRef] = useState('')
  const [systemPrompt, setSystemPrompt] = useState('')

  // Runtime mode fields
  const [runtimeType, setRuntimeType] = useState<'claude' | 'copilot' | 'codex' | 'opencode'>('claude')
  const [maxTurns, setMaxTurns] = useState('50')
  const [allowBash, setAllowBash] = useState(true)
  const [allowedTools, setAllowedTools] = useState(defaultRuntimeAllowedTools)

  const handleRuntimeTypeChange = (value: 'claude' | 'copilot' | 'codex' | 'opencode') => {
    setRuntimeType(value)
    if (value === 'opencode') {
      setAllowBash(false)
      setAllowedTools(defaultOpencodeAllowedTools)
    } else if (runtimeType === 'opencode') {
      setAllowBash(true)
      setAllowedTools(defaultRuntimeAllowedTools)
    }
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()

    const trimmedModel = model.trim()
    if (mode === 'runtime' && runtimeType === 'opencode' && !trimmedModel) {
      toast.error('OpenCode requires an endpoint model ID')
      return
    }

    const parsedMaxTokens = maxTokens.trim() === '' ? undefined : Number(maxTokens)
    const usesMaxTokens = mode === 'ai' || (mode === 'runtime' && runtimeType === 'opencode')
    if (usesMaxTokens && parsedMaxTokens !== undefined && (!Number.isInteger(parsedMaxTokens) || parsedMaxTokens <= 0)) {
      toast.error('Max Tokens must be a positive integer')
      return
    }
    const parsedContextWindow = contextWindow.trim() === '' ? undefined : Number(contextWindow)
    if (mode === 'runtime' && runtimeType === 'opencode' && parsedContextWindow !== undefined &&
      (!Number.isInteger(parsedContextWindow) || parsedContextWindow <= 0)) {
      toast.error('Context Window must be a positive integer')
      return
    }
    if (mode === 'runtime' && runtimeType === 'opencode') {
      const effectiveMaxTokens = parsedMaxTokens ?? defaultOpencodeMaxTokens
      const effectiveContextWindow = parsedContextWindow ?? defaultOpencodeContextWindow
      if (effectiveMaxTokens > maxOpencodeOutputTokens) {
        toast.error('OpenCode Max Output Tokens cannot exceed 32000')
        return
      }
      if (effectiveContextWindow <= effectiveMaxTokens) {
        toast.error('Context Window must be greater than Max Output Tokens')
        return
      }
    }

    const spec: Record<string, unknown> = {}

    if (mode === 'ai') {
      spec.model = {
        provider,
        name: model,
        ...(temperature ? { temperature: parseFloat(temperature) } : {}),
        ...(parsedMaxTokens !== undefined ? { maxTokens: parsedMaxTokens } : {}),
      }
      if (systemPrompt) {
        spec.systemPrompt = { inline: systemPrompt }
      }
    } else {
      spec.runtime = {
        type: runtimeType,
        defaultMaxTurns: parseInt(maxTurns),
        defaultAllowBash: allowBash,
        ...(allowedTools.trim() ? { defaultAllowedTools: allowedTools.split(',').map(t => t.trim()).filter(Boolean) } : {}),
      }
      if (trimmedModel) {
        spec.model = {
          name: trimmedModel,
          ...(runtimeType === 'opencode' && parsedMaxTokens !== undefined ? { maxTokens: parsedMaxTokens } : {}),
          ...(runtimeType === 'opencode' && parsedContextWindow !== undefined ? { contextWindow: parsedContextWindow } : {}),
        }
      }
    }

    if (secretRef && secretRef !== '__none__') {
      spec.secretRef = { name: secretRef }
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
      <PageHeader title="Create Agent" description="Configure a new AI agent" />
      <Card>
        <CardHeader>
          <CardTitle>Agent Configuration</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Name</label>
                <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="my-agent" required />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Mode</label>
                <Select value={mode} onValueChange={(v) => setMode(v as 'ai' | 'runtime')}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ai">AI (LLM Provider)</SelectItem>
                    <SelectItem value="runtime">CLI Runtime (Copilot / Claude / Codex / OpenCode)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            {mode === 'ai' && (
              <div className="space-y-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Provider</label>
                    <Select value={provider} onValueChange={setProvider}>
                      <SelectTrigger><SelectValue placeholder="Select provider" /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="anthropic">Anthropic</SelectItem>
                        <SelectItem value="openai">OpenAI</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Model</label>
                    <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="claude-opus-4-5-20250514" />
                  </div>
                </div>
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Temperature</label>
                    <Input type="number" step="0.1" min="0" max="2" value={temperature} onChange={(e) => setTemperature(e.target.value)} />
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Max Tokens</label>
                    <Input type="number" min="1" step="1" value={maxTokens} onChange={(e) => setMaxTokens(e.target.value)} placeholder="Optional" />
                  </div>
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">System Prompt</label>
                  <textarea
                    className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    value={systemPrompt}
                    onChange={(e) => setSystemPrompt(e.target.value)}
                    placeholder="Optional system prompt..."
                  />
                </div>
              </div>
            )}

            {mode === 'runtime' && (
              <div className="space-y-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Runtime Type</label>
                    <Select value={runtimeType} onValueChange={(v) => handleRuntimeTypeChange(v as 'claude' | 'copilot' | 'codex' | 'opencode')}>
                      <SelectTrigger><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="claude">Claude Code</SelectItem>
                        <SelectItem value="copilot">GitHub Copilot</SelectItem>
                        <SelectItem value="codex">OpenAI Codex</SelectItem>
                        <SelectItem value="opencode">OpenCode</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Max Turns</label>
                    <Input type="number" min="1" max="1000" value={maxTurns} onChange={(e) => setMaxTurns(e.target.value)} />
                  </div>
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Model</label>
                  <Input
                    value={model}
                    onChange={(e) => setModel(e.target.value)}
                    placeholder="Endpoint model ID"
                    required={runtimeType === 'opencode'}
                  />
                  <p className="text-xs text-muted-foreground">
                    Required for OpenCode; optional for runtimes with a configured default model
                  </p>
                </div>
                {runtimeType === 'opencode' && (
                  <div className="grid gap-4 md:grid-cols-2">
                    <div className="space-y-2">
                      <label className="text-sm font-medium">Max Output Tokens</label>
                      <Input
                        type="number"
                        min="1"
                        max="32000"
                        step="1"
                        value={maxTokens}
                        onChange={(e) => setMaxTokens(e.target.value)}
                        placeholder="8192 (default)"
                      />
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium">Context Window</label>
                      <Input
                        type="number"
                        min="1"
                        step="1"
                        value={contextWindow}
                        onChange={(e) => setContextWindow(e.target.value)}
                        placeholder="128000 (default)"
                      />
                    </div>
                  </div>
                )}
                <div className="space-y-2">
                  <label className="text-sm font-medium">Allowed Tools</label>
                  <Input
                    value={allowedTools}
                    onChange={(e) => setAllowedTools(e.target.value)}
                    placeholder={runtimeType === 'opencode' ? defaultOpencodeAllowedTools : defaultRuntimeAllowedTools}
                  />
                  <p className="text-xs text-muted-foreground">Comma-separated list of tool names</p>
                </div>
                <div className="flex items-center gap-2">
                  <Switch checked={allowBash} onCheckedChange={setAllowBash} />
                  <label className="text-sm font-medium">Allow Bash</label>
                </div>
              </div>
            )}

            <div className="space-y-2">
              <label className="text-sm font-medium">Secret Reference</label>
              <Select value={secretRef} onValueChange={setSecretRef}>
                <SelectTrigger><SelectValue placeholder="Select a secret..." /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="__none__">None</SelectItem>
                  {(secretsData?.items ?? []).map((s) => (
                    <SelectItem key={s.name} value={s.name}>{s.name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">Kubernetes Secret containing API keys</p>
            </div>

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
