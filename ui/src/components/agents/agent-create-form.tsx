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
  const [secretRef, setSecretRef] = useState('')
  const [systemPrompt, setSystemPrompt] = useState('')

  // Runtime mode fields
  const [runtimeType, setRuntimeType] = useState<'claude' | 'copilot' | 'codex' | 'opencode'>('claude')
  const [maxTurns, setMaxTurns] = useState('50')
  const [allowBash, setAllowBash] = useState(true)
  const [allowedTools, setAllowedTools] = useState('Read,Glob,Grep,Bash,LS')

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()

    const trimmedModel = model.trim()
    if (mode === 'runtime' && runtimeType === 'opencode' && !trimmedModel) {
      toast.error('OpenCode requires an endpoint model ID')
      return
    }

    const spec: Record<string, unknown> = {}

    if (mode === 'ai') {
      spec.model = {
        provider,
        name: model,
        ...(temperature ? { temperature: parseFloat(temperature) } : {}),
        ...(maxTokens ? { maxTokens: parseInt(maxTokens) } : {}),
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
        spec.model = { name: trimmedModel }
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
                    <Input type="number" value={maxTokens} onChange={(e) => setMaxTokens(e.target.value)} placeholder="Optional" />
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
                    <Select value={runtimeType} onValueChange={(v) => setRuntimeType(v as 'claude' | 'copilot' | 'codex' | 'opencode')}>
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
                <div className="space-y-2">
                  <label className="text-sm font-medium">Allowed Tools</label>
                  <Input value={allowedTools} onChange={(e) => setAllowedTools(e.target.value)} placeholder="Read,Glob,Grep,Bash,LS" />
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
