import { useState, useMemo } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import { useCreateTask } from '@/hooks/use-tasks'
import { useAgentList } from '@/hooks/use-agents'
import { useUIStore } from '@/stores/ui'
import { toast } from 'sonner'

type ValidationErrors = {
  priority?: string
  maxTurns?: string
}

export function TaskCreateForm() {
  const navigate = useNavigate()
  const createTask = useCreateTask()
  const { data: agentsData } = useAgentList()
  const namespace = useUIStore((s) => s.namespace)

  const [name, setName] = useState('')
  const [type, setType] = useState<string>('container')
  const [image, setImage] = useState('')
  const [command, setCommand] = useState('')
  const [provider, setProvider] = useState('')
  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [agentRef, setAgentRef] = useState('')

  // Advanced options
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [priority, setPriority] = useState('')
  const [timeout, setTimeout] = useState('')

  // Agent workspace options
  const [showWorkspace, setShowWorkspace] = useState(false)
  const [gitRepo, setGitRepo] = useState('')
  const [branch, setBranch] = useState('')
  const [pushBranch, setPushBranch] = useState('')
  const [gitSecretRef, setGitSecretRef] = useState('')
  const [maxTurns, setMaxTurns] = useState('')
  const [allowBash, setAllowBash] = useState(false)
  const [validationErrors, setValidationErrors] = useState<ValidationErrors>({})

  const selectedAgent = useMemo(
    () => (agentsData?.items ?? []).find((a) => a.metadata.name === agentRef),
    [agentsData, agentRef],
  )

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const errors: ValidationErrors = {}
    const parseIntegerField = (
      value: string,
      field: keyof ValidationErrors,
      min: number,
      max: number,
      label: string,
    ) => {
      const trimmed = value.trim()
      if (!trimmed) return undefined
      if (!/^\d+$/.test(trimmed)) {
        errors[field] = `${label} must be an integer between ${min} and ${max}.`
        return undefined
      }
      const parsed = Number.parseInt(trimmed, 10)
      if (!Number.isFinite(parsed) || parsed < min || parsed > max) {
        errors[field] = `${label} must be an integer between ${min} and ${max}.`
        return undefined
      }
      return parsed
    }

    const parsedPriority = parseIntegerField(priority, 'priority', 0, 1000, 'Priority')
    const parsedMaxTurns = type === 'agent'
      ? parseIntegerField(maxTurns, 'maxTurns', 1, 1000, 'Max Turns')
      : undefined

    setValidationErrors(errors)
    if (Object.keys(errors).length > 0) {
      toast.error('Please fix form errors before submitting')
      return
    }

    const body: Record<string, unknown> = { name, namespace, type }

    if (type === 'container') {
      body.image = image
      if (command) body.command = command.split(/\s+/).filter(Boolean)
    } else if (type === 'ai') {
      body.ai = { provider, model, prompt }
    } else if (type === 'agent') {
      body.agentRef = { name: agentRef }
      body.prompt = prompt
    }

    if (parsedPriority !== undefined) body.priority = parsedPriority
    if (timeout) body.timeout = timeout

    if (type === 'agent') {
      const workspace: Record<string, unknown> = {}
      if (gitRepo) workspace.gitRepo = gitRepo
      if (branch) workspace.branch = branch
      if (pushBranch) workspace.pushBranch = pushBranch
      if (gitSecretRef) workspace.gitSecretRef = { name: gitSecretRef }
      if (Object.keys(workspace).length > 0) {
        body.agentRuntime = { ...(body.agentRuntime as Record<string, unknown> || {}), workspace }
      }
      if (parsedMaxTurns !== undefined) body.agentRuntime = { ...(body.agentRuntime as Record<string, unknown> || {}), maxTurns: parsedMaxTurns }
      if (allowBash) body.agentRuntime = { ...(body.agentRuntime as Record<string, unknown> || {}), allowBash }
    }

    try {
      await createTask.mutateAsync(body)
      toast.success('Task created')
      navigate({ to: '/tasks' })
    } catch (err) {
      toast.error(`Failed to create task: ${err instanceof Error ? err.message : 'Unknown error'}`)
    }
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-3xl font-bold tracking-tight">Create Task</h1>
        <p className="text-muted-foreground">Create a new task for execution</p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Task Configuration</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Name</label>
                <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="my-task" required />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Type</label>
                <Select
                  value={type}
                  onValueChange={(nextType) => {
                    setType(nextType)
                    if (nextType !== 'agent' && validationErrors.maxTurns) {
                      setValidationErrors((prev) => ({ ...prev, maxTurns: undefined }))
                    }
                  }}
                >
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="container">Container</SelectItem>
                    <SelectItem value="ai">AI</SelectItem>
                    <SelectItem value="agent">Agent</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            {type === 'container' && (
              <div className="space-y-4">
                <div className="space-y-2">
                  <label className="text-sm font-medium">Image</label>
                  <Input value={image} onChange={(e) => setImage(e.target.value)} placeholder="alpine:latest" required />
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Command</label>
                  <Input value={command} onChange={(e) => setCommand(e.target.value)} placeholder="echo hello" />
                </div>
              </div>
            )}

            {type === 'ai' && (
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
                    <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="claude-sonnet-4-20250514" />
                  </div>
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Prompt</label>
                  <textarea
                    className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    value={prompt}
                    onChange={(e) => setPrompt(e.target.value)}
                    placeholder="Enter your prompt..."
                    required
                  />
                </div>
              </div>
            )}

            {type === 'agent' && (
              <div className="space-y-4">
                <div className="space-y-2">
                  <label className="text-sm font-medium">Agent Reference</label>
                  <Select value={agentRef} onValueChange={setAgentRef}>
                    <SelectTrigger><SelectValue placeholder="Select an agent..." /></SelectTrigger>
                    <SelectContent>
                      {(agentsData?.items ?? []).map((a) => (
                        <SelectItem key={a.metadata.name} value={a.metadata.name}>
                          {a.metadata.name}
                          {a.spec.model?.name ? ` (${a.spec.model.name})` : ''}
                          {a.spec.runtime ? ` (${a.spec.runtime.type} runtime)` : ''}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                {selectedAgent && (
                  <div className="rounded-md border border-border bg-muted/50 p-3 text-sm" data-testid="agent-info-card">
                    <div className="flex flex-wrap items-center gap-2">
                      {selectedAgent.spec.model?.provider && (
                        <Badge variant="secondary">{selectedAgent.spec.model.provider}</Badge>
                      )}
                      {selectedAgent.spec.model?.name && (
                        <Badge variant="outline">{selectedAgent.spec.model.name}</Badge>
                      )}
                      {selectedAgent.spec.runtime && (
                        <Badge variant="secondary">{selectedAgent.spec.runtime.type} runtime</Badge>
                      )}
                      {selectedAgent.spec.coordination?.enabled && (
                        <Badge>Coordination</Badge>
                      )}
                      {(selectedAgent.spec.tools?.length ?? 0) > 0 && (
                        <Badge variant="outline">{selectedAgent.spec.tools!.length} tool{selectedAgent.spec.tools!.length !== 1 ? 's' : ''}</Badge>
                      )}
                    </div>
                  </div>
                )}
                <div className="space-y-2">
                  <label className="text-sm font-medium">Prompt</label>
                  <textarea
                    className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    value={prompt}
                    onChange={(e) => setPrompt(e.target.value)}
                    placeholder="Enter your prompt..."
                    required
                  />
                </div>
              </div>
            )}

            <button
              type="button"
              onClick={() => setShowAdvanced(!showAdvanced)}
              className="text-sm text-muted-foreground hover:text-foreground flex items-center gap-1"
            >
              {showAdvanced ? '▼' : '▶'} Advanced Options
            </button>
            {showAdvanced && (
              <div className="space-y-4 border-l-2 border-border pl-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Priority</label>
                    <Input
                      type="number"
                      min={0}
                      max={1000}
                      value={priority}
                      onChange={(e) => {
                        setPriority(e.target.value)
                        if (validationErrors.priority) {
                          setValidationErrors((prev) => ({ ...prev, priority: undefined }))
                        }
                      }}
                      placeholder="500"
                    />
                    {validationErrors.priority && (
                      <p className="text-xs text-destructive">{validationErrors.priority}</p>
                    )}
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Timeout</label>
                    <Input
                      value={timeout}
                      onChange={(e) => setTimeout(e.target.value)}
                      placeholder="30m"
                    />
                  </div>
                </div>

                {type === 'agent' && (
                  <>
                    <button
                      type="button"
                      onClick={() => setShowWorkspace(!showWorkspace)}
                      className="text-sm text-muted-foreground hover:text-foreground flex items-center gap-1"
                    >
                      {showWorkspace ? '▼' : '▶'} Workspace Configuration
                    </button>
                    {showWorkspace && (
                      <div className="space-y-4 border-l-2 border-border pl-4">
                        <div className="grid gap-4 md:grid-cols-2">
                          <div className="space-y-2">
                            <label className="text-sm font-medium">Git Repo URL</label>
                            <Input
                              value={gitRepo}
                              onChange={(e) => setGitRepo(e.target.value)}
                              placeholder="https://github.com/org/repo"
                            />
                          </div>
                          <div className="space-y-2">
                            <label className="text-sm font-medium">Branch</label>
                            <Input
                              value={branch}
                              onChange={(e) => setBranch(e.target.value)}
                              placeholder="main"
                            />
                          </div>
                        </div>
                        <div className="grid gap-4 md:grid-cols-2">
                          <div className="space-y-2">
                            <label className="text-sm font-medium">Push Branch</label>
                            <Input
                              value={pushBranch}
                              onChange={(e) => setPushBranch(e.target.value)}
                              placeholder="feature/my-task"
                            />
                          </div>
                          <div className="space-y-2">
                            <label className="text-sm font-medium">Git Secret Ref</label>
                            <Input
                              value={gitSecretRef}
                              onChange={(e) => setGitSecretRef(e.target.value)}
                              placeholder="git-credentials"
                            />
                          </div>
                        </div>
                      </div>
                    )}
                    <div className="grid gap-4 md:grid-cols-2">
                      <div className="space-y-2">
                        <label className="text-sm font-medium">Max Turns</label>
                        <Input
                          type="number"
                          min={1}
                          value={maxTurns}
                          onChange={(e) => {
                            setMaxTurns(e.target.value)
                            if (validationErrors.maxTurns) {
                              setValidationErrors((prev) => ({ ...prev, maxTurns: undefined }))
                            }
                          }}
                          placeholder="10"
                        />
                        {validationErrors.maxTurns && (
                          <p className="text-xs text-destructive">{validationErrors.maxTurns}</p>
                        )}
                      </div>
                      <div className="space-y-2">
                        <label className="text-sm font-medium">Allow Bash</label>
                        <div className="flex items-center gap-2 pt-1">
                          <Switch checked={allowBash} onCheckedChange={setAllowBash} />
                          <span className="text-sm text-muted-foreground">{allowBash ? 'Enabled' : 'Disabled'}</span>
                        </div>
                      </div>
                    </div>
                  </>
                )}
              </div>
            )}

            <div className="flex gap-2">
              <Button type="submit" disabled={createTask.isPending}>
                {createTask.isPending ? 'Creating...' : 'Create Task'}
              </Button>
              <Button type="button" variant="outline" onClick={() => navigate({ to: '/tasks' })}>
                Cancel
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
