import { useMemo, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ManifestEditor } from '@/components/ui/manifest-editor'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import { PageHeader } from '@/components/layout/page-header'
import { useCreateTask } from '@/hooks/use-tasks'
import { useAgentList } from '@/hooks/use-agents'
import { useProviderList } from '@/hooks/use-providers'
import { useUIStore } from '@/stores/ui'
import { toast } from 'sonner'
import { workspaceConfigSchema, type WorkspaceIntent } from '@/schemas/task'
import { builtInAgentRuntimeLabel } from '@/lib/agent-runtime'

// Registered Provider CRs are referenced by name; the two direct values keep
// the inline-provider path.
const TASK_PROVIDER_REF_PREFIX = 'ref:'

function optionalRepositoryIdentity(provider: string, id: string) {
  return provider.trim() && id.trim() ? { provider: provider.trim(), id: id.trim() } : undefined
}

function optionalCredentialReference(name: string, key: string) {
  const trimmedName = name.trim()
  if (!trimmedName) return undefined
  const trimmedKey = key.trim()
  return trimmedKey ? { name: trimmedName, key: trimmedKey } : { name: trimmedName }
}

function splitList(raw: string): string[] {
  return raw.split(',').map((entry) => entry.trim()).filter(Boolean)
}

// KEY=VALUE per line → EnvVar[].
function parseEnvLines(raw: string): Array<{ name: string; value: string }> {
  return raw
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      const separator = line.indexOf('=')
      if (separator === -1) return { name: line, value: '' }
      return { name: line.slice(0, separator).trim(), value: line.slice(separator + 1) }
    })
    .filter((entry) => entry.name)
}

export function TaskCreateForm() {
  const navigate = useNavigate()
  const createTask = useCreateTask()
  const { data: agentsData } = useAgentList()
  const { data: providersData } = useProviderList(false)
  const namespace = useUIStore((s) => s.namespace)

  const [name, setName] = useState('')
  const [type, setType] = useState<string>('container')
  const [image, setImage] = useState('')
  const [command, setCommand] = useState('')
  const [provider, setProvider] = useState('')
  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [agentRef, setAgentRef] = useState('')

  const [showAdvanced, setShowAdvanced] = useState(false)
  const [priority, setPriority] = useState('')
  const [timeout, setTimeout] = useState('')

  // AI-specific extras.
  const [systemPrompt, setSystemPrompt] = useState('')
  const [temperature, setTemperature] = useState('')
  const [maxTokens, setMaxTokens] = useState('')
  const [aiTools, setAITools] = useState('')
  const [aiSkills, setAISkills] = useState('')

  // Scheduling (any type): a cron expression turns the task into a durable
  // scheduled parent that mints child runs.
  const [schedule, setSchedule] = useState('')
  const [timeZone, setTimeZone] = useState('')
  const [concurrencyPolicy, setConcurrencyPolicy] = useState('')
  const [suspend, setSuspend] = useState(false)

  // Execution extras.
  const [envText, setEnvText] = useState('')
  const [argsText, setArgsText] = useState('')
  const [webhookURL, setWebhookURL] = useState('')
  const [secretRefName, setSecretRefName] = useState('')
  const [retryMax, setRetryMax] = useState('')
  const [retryInitialDelay, setRetryInitialDelay] = useState('')
  const [retryBackoff, setRetryBackoff] = useState('')

  // Session continuation.
  const [sessionName, setSessionName] = useState('')
  const [sessionCreate, setSessionCreate] = useState(false)
  const [sessionMaxMessages, setSessionMaxMessages] = useState('')

  // Agent runtime overrides.
  const [maxTurns, setMaxTurns] = useState('')
  const [allowedTools, setAllowedTools] = useState('')
  const [disallowedTools, setDisallowedTools] = useState('')
  const [allowBash, setAllowBash] = useState(false)

  // Clean-room publication policies (write intent).
  const [expectedRemoteSHA, setExpectedRemoteSHA] = useState('')
  const [maxChangedFiles, setMaxChangedFiles] = useState('')
  const [allowedPaths, setAllowedPaths] = useState('')
  const [denyRepositoryControlPaths, setDenyRepositoryControlPaths] = useState(false)
  const [rejectBinaryFiles, setRejectBinaryFiles] = useState(false)
  const [rejectSecretLikeContent, setRejectSecretLikeContent] = useState(false)

  const [yamlOpen, setYamlOpen] = useState(false)

  const [showWorkspace, setShowWorkspace] = useState(false)
  const [workspaceIntent, setWorkspaceIntent] = useState<WorkspaceIntent>('read')
  const [gitRepo, setGitRepo] = useState('')
  const [sourceProvider, setSourceProvider] = useState('')
  const [sourceRepositoryID, setSourceRepositoryID] = useState('')
  const [branch, setBranch] = useState('')
  const [gitRef, setGitRef] = useState('')
  const [subPath, setSubPath] = useState('')
  const [readCredentialName, setReadCredentialName] = useState('')
  const [readCredentialKey, setReadCredentialKey] = useState('')
  const [publicationGitRepo, setPublicationGitRepo] = useState('')
  const [publicationProvider, setPublicationProvider] = useState('')
  const [publicationRepositoryID, setPublicationRepositoryID] = useState('')
  const [publicationReadCredentialName, setPublicationReadCredentialName] = useState('')
  const [publicationReadCredentialKey, setPublicationReadCredentialKey] = useState('')
  const [publicationCredentialName, setPublicationCredentialName] = useState('')
  const [publicationCredentialKey, setPublicationCredentialKey] = useState('')
  const [forgeCredentialName, setForgeCredentialName] = useState('')
  const [forgeCredentialKey, setForgeCredentialKey] = useState('')
  const [pushBranch, setPushBranch] = useState('')
  const [prBaseBranch, setPRBaseBranch] = useState('')
  const [createPR, setCreatePR] = useState(false)

  const dispatchableAgents = useMemo(
    () => (agentsData?.items ?? []).filter((agent) => agent.spec.runtime && 'type' in agent.spec.runtime),
    [agentsData],
  )
  const externalRuntimeAgentCount = (agentsData?.items.length ?? 0) - dispatchableAgents.length
  const selectedAgent = useMemo(
    () => dispatchableAgents.find((agent) => agent.metadata.name === agentRef),
    [agentRef, dispatchableAgents],
  )

  const selectedRuntime = selectedAgent?.spec.runtime
  const selectedRuntimeLabel = selectedRuntime
    ? 'type' in selectedRuntime
      ? builtInAgentRuntimeLabel(selectedRuntime.type)
      : `AgentRuntime ${selectedRuntime.runtimeRef.name}`
    : undefined

  // Builds the CreateTaskRequest body from form state, or returns null after
  // toasting the first validation problem.
  const buildBody = (silent = false): Record<string, unknown> | null => {
    const body: Record<string, unknown> = { name, namespace, type }

    if (type === 'container') {
      body.image = image
      if (command) body.command = command.split(' ')
      const args = splitList(argsText)
      if (args.length) body.args = args
    } else if (type === 'ai') {
      const ai: Record<string, unknown> = { model, prompt }
      if (provider.startsWith(TASK_PROVIDER_REF_PREFIX)) {
        ai.providerRef = { name: provider.slice(TASK_PROVIDER_REF_PREFIX.length) }
      } else if (provider) {
        ai.provider = provider
      }
      if (systemPrompt.trim()) ai.systemPrompt = systemPrompt
      if (temperature) ai.temperature = parseFloat(temperature)
      if (maxTokens) ai.maxTokens = parseInt(maxTokens)
      const tools = splitList(aiTools)
      if (tools.length) ai.tools = tools
      const skills = splitList(aiSkills)
      if (skills.length) ai.skills = skills.map((skillName) => ({ name: skillName }))
      body.ai = ai
    } else if (type === 'agent') {
      body.agentRef = { name: agentRef }
      body.prompt = prompt

      const credentialInputs = [
        { label: 'Read credential', name: readCredentialName, key: readCredentialKey },
        ...(workspaceIntent === 'write'
          ? [
              { label: 'Publication read credential', name: publicationReadCredentialName, key: publicationReadCredentialKey },
              { label: 'Publication write credential', name: publicationCredentialName, key: publicationCredentialKey },
              { label: 'Forge credential', name: forgeCredentialName, key: forgeCredentialKey },
            ]
          : []),
      ]
      const credentialWithoutName = credentialInputs.find(({ name: credentialName, key }) => key.trim() && !credentialName.trim())
      if (credentialWithoutName) {
        if (!silent) toast.error(`${credentialWithoutName.label} Secret is required when a key is set`)
        return null
      }
      if (workspaceIntent === 'write' && !gitRepo.trim()) {
        if (!silent) toast.error('Source repository URL is required for write workspaces')
        return null
      }
      if (workspaceIntent === 'write' && !publicationCredentialName.trim()) {
        if (!silent) toast.error('Publication write credential Secret is required for write workspaces')
        return null
      }
      if (workspaceIntent === 'write' && createPR && !prBaseBranch.trim()) {
        if (!silent) toast.error('Pull request base branch is required when creating a pull request')
        return null
      }
      if (workspaceIntent === 'write' && createPR && !forgeCredentialName.trim()) {
        if (!silent) toast.error('Forge credential Secret is required when creating a pull request')
        return null
      }

      const workspace: Record<string, unknown> = { intent: workspaceIntent }
      if (gitRepo.trim()) workspace.gitRepo = gitRepo.trim()
      const sourceRepository = optionalRepositoryIdentity(sourceProvider, sourceRepositoryID)
      if (sourceRepository) workspace.sourceRepository = sourceRepository
      if (branch.trim()) workspace.branch = branch.trim()
      if (gitRef.trim()) workspace.ref = gitRef.trim()
      if (subPath.trim()) workspace.subPath = subPath.trim()
      const readCredentialRef = optionalCredentialReference(readCredentialName, readCredentialKey)
      if (readCredentialRef) workspace.readCredentialRef = readCredentialRef

      if (workspaceIntent === 'write') {
        if (expectedRemoteSHA.trim()) workspace.expectedRemoteSHA = expectedRemoteSHA.trim()
        if (maxChangedFiles) workspace.maxChangedFiles = parseInt(maxChangedFiles)
        const parsedAllowedPaths = splitList(allowedPaths)
        if (parsedAllowedPaths.length) workspace.allowedPaths = parsedAllowedPaths
        if (denyRepositoryControlPaths) workspace.denyRepositoryControlPaths = true
        if (rejectBinaryFiles) workspace.rejectBinaryFiles = true
        if (rejectSecretLikeContent) workspace.rejectSecretLikeContent = true
        if (publicationGitRepo.trim()) workspace.publicationGitRepo = publicationGitRepo.trim()
        const publicationRepository = optionalRepositoryIdentity(publicationProvider, publicationRepositoryID)
        if (publicationRepository) workspace.publicationRepository = publicationRepository
        const publicationReadCredentialRef = optionalCredentialReference(
          publicationReadCredentialName,
          publicationReadCredentialKey,
        )
        if (publicationReadCredentialRef) workspace.publicationReadCredentialRef = publicationReadCredentialRef
        const publicationCredentialRef = optionalCredentialReference(publicationCredentialName, publicationCredentialKey)
        if (publicationCredentialRef) workspace.publicationCredentialRef = publicationCredentialRef
        const forgeCredentialRef = optionalCredentialReference(forgeCredentialName, forgeCredentialKey)
        if (forgeCredentialRef) workspace.forgeCredentialRef = forgeCredentialRef
        if (pushBranch.trim()) workspace.pushBranch = pushBranch.trim()
        if (prBaseBranch.trim()) workspace.prBaseBranch = prBaseBranch.trim()
        if (createPR) workspace.createPR = true
      }
      const workspaceResult = workspaceConfigSchema.safeParse(workspace)
      if (!workspaceResult.success) {
        if (!silent) toast.error(workspaceResult.error.issues[0]?.message ?? 'Invalid workspace configuration')
        return null
      }
      body.workspace = workspaceResult.data

      const agentRuntime: Record<string, unknown> = {}
      if (maxTurns) agentRuntime.maxTurns = parseInt(maxTurns)
      const parsedAllowedTools = splitList(allowedTools)
      if (parsedAllowedTools.length) agentRuntime.allowedTools = parsedAllowedTools
      const parsedDisallowedTools = splitList(disallowedTools)
      if (parsedDisallowedTools.length) agentRuntime.disallowedTools = parsedDisallowedTools
      if (allowBash) agentRuntime.allowBash = true
      if (Object.keys(agentRuntime).length) body.agentRuntime = agentRuntime
    }

    if (priority) body.priority = parseInt(priority)
    if (timeout) body.timeout = timeout

    const env = parseEnvLines(envText)
    if (env.length) body.env = env
    if (webhookURL.trim()) body.webhookURL = webhookURL.trim()
    if (secretRefName.trim()) body.secretRef = { name: secretRefName.trim() }
    if (retryMax || retryInitialDelay || retryBackoff) {
      body.retryPolicy = {
        ...(retryMax ? { maxRetries: parseInt(retryMax) } : {}),
        ...(retryInitialDelay ? { initialDelay: retryInitialDelay } : {}),
        ...(retryBackoff ? { backoffMultiplier: parseFloat(retryBackoff) } : {}),
      }
    }
    if (sessionName.trim()) {
      body.sessionRef = {
        name: sessionName.trim(),
        ...(sessionCreate ? { create: true } : {}),
        ...(sessionMaxMessages ? { maxMessages: parseInt(sessionMaxMessages) } : {}),
      }
    }
    if (schedule.trim()) {
      body.schedule = schedule.trim()
      if (timeZone.trim()) body.timeZone = timeZone.trim()
      if (concurrencyPolicy) body.concurrencyPolicy = concurrencyPolicy
      if (suspend) body.suspend = true
    }

    return body
  }

  const submitBody = async (body: Record<string, unknown>) => {
    try {
      await createTask.mutateAsync(body)
      toast.success('Task created')
      navigate({ to: '/tasks' })
      return true
    } catch (err) {
      toast.error(`Failed to create task: ${err instanceof Error ? err.message : 'Unknown error'}`)
      return false
    }
  }

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault()
    const body = buildBody()
    if (!body) return
    await submitBody(body)
  }

  return (
    <div className="space-y-4">
      <PageHeader
        title="Create Task"
        description="Create a new task for execution"
        action={
          <Button variant="outline" onClick={() => setYamlOpen(true)}>
            Edit as YAML
          </Button>
        }
      />
      <ManifestEditor
        open={yamlOpen}
        onOpenChange={setYamlOpen}
        title="Create task from YAML"
        description="Full CreateTaskRequest body — every TaskSpec field is available here, including fields the form does not cover. requestedBy and transaction are server-stamped and must not appear."
        initialValue={buildBody(true) ?? { name: name || 'my-task', namespace, type }}
        submitLabel="Create task"
        pending={createTask.isPending}
        onSubmit={async (manifest) => {
          const created = await submitBody(manifest)
          if (created) setYamlOpen(false)
        }}
      />
      <Card>
        <CardHeader>
          <CardTitle>Task Configuration</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label htmlFor="task-name" className="text-sm font-medium">Name</label>
                <Input id="task-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-task" required />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Type</label>
                <Select value={type} onValueChange={setType}>
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
                  <label htmlFor="task-image" className="text-sm font-medium">Image</label>
                  <Input id="task-image" value={image} onChange={(e) => setImage(e.target.value)} placeholder="alpine:latest" required />
                </div>
                <div className="space-y-2">
                  <label htmlFor="task-command" className="text-sm font-medium">Command</label>
                  <Input id="task-command" value={command} onChange={(e) => setCommand(e.target.value)} placeholder="echo hello" />
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
                        {(providersData?.items ?? []).map((registered) => (
                          <SelectItem key={registered.name} value={`${TASK_PROVIDER_REF_PREFIX}${registered.name}`}>
                            {registered.name} (Provider)
                          </SelectItem>
                        ))}
                        <SelectItem value="anthropic">Anthropic</SelectItem>
                        <SelectItem value="openai">OpenAI</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="task-model" className="text-sm font-medium">Model</label>
                    <Input id="task-model" value={model} onChange={(e) => setModel(e.target.value)} placeholder="claude-sonnet-4-20250514" />
                  </div>
                </div>
                <div className="space-y-2">
                  <label htmlFor="ai-prompt" className="text-sm font-medium">Prompt</label>
                  <textarea
                    id="ai-prompt"
                    className="flex min-h-32 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    value={prompt}
                    onChange={(e) => setPrompt(e.target.value)}
                    placeholder="Enter your prompt..."
                    required
                  />
                </div>
                <div className="space-y-2">
                  <label htmlFor="ai-system-prompt" className="text-sm font-medium">System prompt (optional)</label>
                  <textarea
                    id="ai-system-prompt"
                    className="flex min-h-20 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    value={systemPrompt}
                    onChange={(e) => setSystemPrompt(e.target.value)}
                    placeholder="Overrides the agent/provider default system prompt"
                  />
                </div>
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label htmlFor="ai-temperature" className="text-sm font-medium">Temperature (optional)</label>
                    <Input id="ai-temperature" type="number" step="0.1" min="0" max="2" value={temperature} onChange={(e) => setTemperature(e.target.value)} />
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="ai-max-tokens" className="text-sm font-medium">Max tokens (optional)</label>
                    <Input id="ai-max-tokens" type="number" min="1" value={maxTokens} onChange={(e) => setMaxTokens(e.target.value)} />
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="ai-tools" className="text-sm font-medium">Tools (comma-separated, optional)</label>
                    <Input id="ai-tools" value={aiTools} onChange={(e) => setAITools(e.target.value)} placeholder="web_search, code_exec" />
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="ai-skills" className="text-sm font-medium">Skills (comma-separated, optional)</label>
                    <Input id="ai-skills" value={aiSkills} onChange={(e) => setAISkills(e.target.value)} placeholder="code-review" />
                  </div>
                </div>
              </div>
            )}

            {type === 'agent' && (
              <div className="space-y-4">
                <div className="space-y-2">
                  <label className="text-sm font-medium">Agent Reference</label>
                  <Select value={agentRef} onValueChange={setAgentRef} disabled={dispatchableAgents.length === 0}>
                    <SelectTrigger><SelectValue placeholder="Select an agent" /></SelectTrigger>
                    <SelectContent>
                      {dispatchableAgents.map((agent) => (
                        <SelectItem key={agent.metadata.name} value={agent.metadata.name}>
                          {agent.metadata.name}
                          {agent.spec.runtime && ` (${ 'type' in agent.spec.runtime ? builtInAgentRuntimeLabel(agent.spec.runtime.type) : agent.spec.runtime.runtimeRef.name })`}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  {externalRuntimeAgentCount > 0 && (
                    <p className="text-xs text-muted-foreground">
                      Agents without a built-in CLI runtime are hidden because only built-in ACP Task dispatch is available.
                    </p>
                  )}
                </div>

                {selectedAgent && (
                  <div className="rounded-md border bg-muted/50 p-3 text-sm" data-testid="agent-info-card">
                    <div className="flex items-center gap-2 font-medium">
                      {selectedAgent.metadata.name}
                      {selectedRuntimeLabel && <Badge variant="secondary">{selectedRuntimeLabel}</Badge>}
                    </div>
                    <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-muted-foreground">
                      {selectedAgent.spec.model?.provider && <span>{selectedAgent.spec.model.provider}</span>}
                      {selectedAgent.spec.model?.name && <span>{selectedAgent.spec.model.name}</span>}
                      {selectedAgent.spec.coordination?.enabled && <Badge variant="outline">Coordination</Badge>}
                      {(selectedAgent.spec.tools?.length ?? 0) > 0 && <span>{selectedAgent.spec.tools!.length} tools</span>}
                    </div>
                  </div>
                )}

                <div className="space-y-2">
                  <label htmlFor="agent-prompt" className="text-sm font-medium">Prompt</label>
                  <textarea
                    id="agent-prompt"
                    className="flex min-h-32 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
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
              className="flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
            >
              {showAdvanced ? '▼' : '▶'} Advanced Options
            </button>

            {showAdvanced && (
              <div className="space-y-4 border-l-2 border-border pl-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label htmlFor="task-priority" className="text-sm font-medium">Priority</label>
                    <Input id="task-priority" type="number" min={0} max={1000} value={priority} onChange={(e) => setPriority(e.target.value)} placeholder="500" />
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="task-timeout" className="text-sm font-medium">Timeout</label>
                    <Input id="task-timeout" value={timeout} onChange={(e) => setTimeout(e.target.value)} placeholder="30m" />
                  </div>
                </div>

                <div className="space-y-3 rounded-md border bg-muted/20 p-4">
                  <p className="text-sm font-medium">Schedule (optional)</p>
                  <p className="text-xs text-muted-foreground">
                    A cron expression keeps this task as a Scheduled parent; each tick creates a child run.
                  </p>
                  <div className="grid gap-4 md:grid-cols-3">
                    <div className="space-y-2">
                      <label htmlFor="task-schedule" className="text-sm font-medium">Cron</label>
                      <Input id="task-schedule" value={schedule} onChange={(e) => setSchedule(e.target.value)} placeholder="0 9 * * 1-5" />
                    </div>
                    <div className="space-y-2">
                      <label htmlFor="task-timezone" className="text-sm font-medium">Time zone</label>
                      <Input id="task-timezone" value={timeZone} onChange={(e) => setTimeZone(e.target.value)} placeholder="America/Los_Angeles" disabled={!schedule.trim()} />
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium">Concurrency</label>
                      <Select value={concurrencyPolicy} onValueChange={setConcurrencyPolicy} disabled={!schedule.trim()}>
                        <SelectTrigger aria-label="Concurrency policy"><SelectValue placeholder="Forbid (default)" /></SelectTrigger>
                        <SelectContent>
                          <SelectItem value="Forbid">Forbid</SelectItem>
                          <SelectItem value="Allow">Allow</SelectItem>
                        </SelectContent>
                      </Select>
                    </div>
                  </div>
                  <label className="flex items-center gap-2 text-sm">
                    <Switch checked={suspend} onCheckedChange={setSuspend} disabled={!schedule.trim()} aria-label="Create suspended" />
                    Create suspended (no runs until resumed with kubectl)
                  </label>
                </div>

                <div className="space-y-3 rounded-md border bg-muted/20 p-4">
                  <p className="text-sm font-medium">Execution (optional)</p>
                  <div className="grid gap-4 md:grid-cols-2">
                    {type === 'container' && (
                      <div className="space-y-2">
                        <label htmlFor="task-args" className="text-sm font-medium">Args (comma-separated)</label>
                        <Input id="task-args" value={argsText} onChange={(e) => setArgsText(e.target.value)} placeholder="--verbose, --once" />
                      </div>
                    )}
                    <div className="space-y-2">
                      <label htmlFor="task-webhook" className="text-sm font-medium">Webhook URL</label>
                      <Input id="task-webhook" value={webhookURL} onChange={(e) => setWebhookURL(e.target.value)} placeholder="https://hooks.example.com/orka" />
                    </div>
                    <div className="space-y-2">
                      <label htmlFor="task-secret-ref" className="text-sm font-medium">Secret ref</label>
                      <Input id="task-secret-ref" value={secretRefName} onChange={(e) => setSecretRefName(e.target.value)} placeholder="task-credentials" />
                    </div>
                  </div>
                  <div className="space-y-2">
                    <label htmlFor="task-env" className="text-sm font-medium">Environment (KEY=VALUE per line)</label>
                    <textarea
                      id="task-env"
                      className="flex min-h-16 w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-xs ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                      value={envText}
                      onChange={(e) => setEnvText(e.target.value)}
                      placeholder={'LOG_LEVEL=debug\nREGION=us-west-2'}
                    />
                  </div>
                  <div className="grid gap-4 md:grid-cols-3">
                    <div className="space-y-2">
                      <label htmlFor="task-retry-max" className="text-sm font-medium">Max retries</label>
                      <Input id="task-retry-max" type="number" min="0" value={retryMax} onChange={(e) => setRetryMax(e.target.value)} />
                    </div>
                    <div className="space-y-2">
                      <label htmlFor="task-retry-delay" className="text-sm font-medium">Initial delay</label>
                      <Input id="task-retry-delay" value={retryInitialDelay} onChange={(e) => setRetryInitialDelay(e.target.value)} placeholder="10s" />
                    </div>
                    <div className="space-y-2">
                      <label htmlFor="task-retry-backoff" className="text-sm font-medium">Backoff multiplier</label>
                      <Input id="task-retry-backoff" type="number" step="0.1" min="1" value={retryBackoff} onChange={(e) => setRetryBackoff(e.target.value)} placeholder="2" />
                    </div>
                  </div>
                </div>

                <div className="space-y-3 rounded-md border bg-muted/20 p-4">
                  <p className="text-sm font-medium">Session (optional)</p>
                  <p className="text-xs text-muted-foreground">
                    Attach to a session to continue an earlier conversation with the same context.
                  </p>
                  <div className="grid gap-4 md:grid-cols-3">
                    <div className="space-y-2">
                      <label htmlFor="task-session-name" className="text-sm font-medium">Session name</label>
                      <Input id="task-session-name" value={sessionName} onChange={(e) => setSessionName(e.target.value)} placeholder="feature-discussion" />
                    </div>
                    <div className="space-y-2">
                      <label htmlFor="task-session-max" className="text-sm font-medium">Max messages</label>
                      <Input id="task-session-max" type="number" min="1" value={sessionMaxMessages} onChange={(e) => setSessionMaxMessages(e.target.value)} placeholder="50" disabled={!sessionName.trim()} />
                    </div>
                    <label className="flex items-center gap-2 self-end pb-2 text-sm">
                      <Switch checked={sessionCreate} onCheckedChange={setSessionCreate} disabled={!sessionName.trim()} aria-label="Create session if missing" />
                      Create if missing
                    </label>
                  </div>
                </div>

                {type === 'agent' && (
                  <div className="space-y-3 rounded-md border bg-muted/20 p-4">
                    <p className="text-sm font-medium">Runtime overrides (optional)</p>
                    <div className="grid gap-4 md:grid-cols-3">
                      <div className="space-y-2">
                        <label htmlFor="task-max-turns" className="text-sm font-medium">Max turns</label>
                        <Input id="task-max-turns" type="number" min="1" max="1000" value={maxTurns} onChange={(e) => setMaxTurns(e.target.value)} />
                      </div>
                      <div className="space-y-2">
                        <label htmlFor="task-allowed-tools" className="text-sm font-medium">Allowed tools</label>
                        <Input id="task-allowed-tools" value={allowedTools} onChange={(e) => setAllowedTools(e.target.value)} placeholder="Read, Grep" />
                      </div>
                      <div className="space-y-2">
                        <label htmlFor="task-disallowed-tools" className="text-sm font-medium">Disallowed tools</label>
                        <Input id="task-disallowed-tools" value={disallowedTools} onChange={(e) => setDisallowedTools(e.target.value)} placeholder="WebFetch" />
                      </div>
                    </div>
                    <label className="flex items-center gap-2 text-sm">
                      <Switch checked={allowBash} onCheckedChange={setAllowBash} aria-label="Allow bash" />
                      Allow bash
                    </label>
                  </div>
                )}

                {type === 'agent' && (
                  <>
                    <button
                      type="button"
                      onClick={() => setShowWorkspace(!showWorkspace)}
                      className="flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
                    >
                      {showWorkspace ? '▼' : '▶'} Workspace policy
                    </button>
                    {showWorkspace && (
                      <div className="space-y-5 rounded-md border bg-muted/20 p-4">
                        <div className="space-y-2">
                          <label className="text-sm font-medium">Workspace intent</label>
                          <Select
                            value={workspaceIntent}
                            onValueChange={(value) => {
                              const intent = value as WorkspaceIntent
                              setWorkspaceIntent(intent)
                              if (intent === 'read') setCreatePR(false)
                            }}
                          >
                            <SelectTrigger><SelectValue /></SelectTrigger>
                            <SelectContent>
                              <SelectItem value="read">Read — verified workspace must remain unchanged</SelectItem>
                              <SelectItem value="write">Write — produce a validated publication artifact</SelectItem>
                            </SelectContent>
                          </Select>
                        </div>

                        <div className="grid gap-4 md:grid-cols-2">
                          <div className="space-y-2">
                            <label htmlFor="workspace-source-url" className="text-sm font-medium">Source repository URL</label>
                            <Input id="workspace-source-url" value={gitRepo} onChange={(e) => setGitRepo(e.target.value)} placeholder="https://github.com/org/repo" required={workspaceIntent === 'write'} />
                          </div>
                          <div className="space-y-2">
                            <label htmlFor="workspace-read-credential" className="text-sm font-medium">Read credential Secret</label>
                            <Input id="workspace-read-credential" value={readCredentialName} onChange={(e) => setReadCredentialName(e.target.value)} placeholder="source-read" />
                          </div>
                          <div className="space-y-2">
                            <label htmlFor="workspace-read-credential-key" className="text-sm font-medium">Read credential key</label>
                            <Input id="workspace-read-credential-key" value={readCredentialKey} onChange={(e) => setReadCredentialKey(e.target.value)} placeholder="token (default)" />
                          </div>
                        </div>
                        <div className="grid gap-4 md:grid-cols-2">
                          <div className="space-y-2">
                            <label htmlFor="source-provider" className="text-sm font-medium">Source repository provider</label>
                            <Input id="source-provider" value={sourceProvider} onChange={(e) => setSourceProvider(e.target.value)} placeholder="github" />
                          </div>
                          <div className="space-y-2">
                            <label htmlFor="source-repository-id" className="text-sm font-medium">Source repository URL identity</label>
                            <Input id="source-repository-id" value={sourceRepositoryID} onChange={(e) => setSourceRepositoryID(e.target.value)} placeholder="github.com/org/repo" />
                            <p className="text-xs text-muted-foreground">Use the normalized credential-free URL identity, not a GitHub node ID.</p>
                          </div>
                        </div>
                        <div className="grid gap-4 md:grid-cols-3">
                          <div className="space-y-2">
                            <label htmlFor="workspace-branch" className="text-sm font-medium">Source branch</label>
                            <Input id="workspace-branch" value={branch} onChange={(e) => setBranch(e.target.value)} placeholder="main" />
                          </div>
                          <div className="space-y-2">
                            <label htmlFor="workspace-ref" className="text-sm font-medium">Source ref</label>
                            <Input id="workspace-ref" value={gitRef} onChange={(e) => setGitRef(e.target.value)} placeholder="commit, tag, or ref" />
                          </div>
                          <div className="space-y-2">
                            <label htmlFor="workspace-subpath" className="text-sm font-medium">Subpath</label>
                            <Input id="workspace-subpath" value={subPath} onChange={(e) => setSubPath(e.target.value)} placeholder="services/api" />
                          </div>
                        </div>

                        {workspaceIntent === 'write' && (
                          <div className="space-y-4 border-t pt-4">
                            <div>
                              <p className="text-sm font-medium">Publication</p>
                              <p className="text-xs text-muted-foreground">A clean-room publisher prepares, pushes with exact-ref CAS, and independently verifies the branch.</p>
                            </div>
                            <div className="grid gap-4 md:grid-cols-2">
                              <div className="space-y-2">
                                <label htmlFor="publication-url" className="text-sm font-medium">Publication repository URL</label>
                                <Input id="publication-url" value={publicationGitRepo} onChange={(e) => setPublicationGitRepo(e.target.value)} placeholder="https://github.com/org/repo" />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="publication-read-credential" className="text-sm font-medium">Publication read credential Secret</label>
                                <Input id="publication-read-credential" value={publicationReadCredentialName} onChange={(e) => setPublicationReadCredentialName(e.target.value)} placeholder="target-read" />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="publication-read-credential-key" className="text-sm font-medium">Publication read credential key</label>
                                <Input id="publication-read-credential-key" value={publicationReadCredentialKey} onChange={(e) => setPublicationReadCredentialKey(e.target.value)} placeholder="token (default)" />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="publication-write-credential" className="text-sm font-medium">Publication write credential Secret</label>
                                <Input id="publication-write-credential" value={publicationCredentialName} onChange={(e) => setPublicationCredentialName(e.target.value)} placeholder="target-write" required={workspaceIntent === 'write'} />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="publication-write-credential-key" className="text-sm font-medium">Publication write credential key</label>
                                <Input id="publication-write-credential-key" value={publicationCredentialKey} onChange={(e) => setPublicationCredentialKey(e.target.value)} placeholder="token (default)" />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="forge-credential" className="text-sm font-medium">Forge credential Secret</label>
                                <Input id="forge-credential" value={forgeCredentialName} onChange={(e) => setForgeCredentialName(e.target.value)} placeholder="forge-api" required={workspaceIntent === 'write' && createPR} />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="forge-credential-key" className="text-sm font-medium">Forge credential key</label>
                                <Input id="forge-credential-key" value={forgeCredentialKey} onChange={(e) => setForgeCredentialKey(e.target.value)} placeholder="token (default)" />
                              </div>
                            </div>
                            <p className="text-xs text-muted-foreground">Only Secret names and keys are submitted. Secret values are never shown in this form.</p>
                            <div className="grid gap-4 md:grid-cols-2">
                              <div className="space-y-2">
                                <label htmlFor="publication-provider" className="text-sm font-medium">Publication provider</label>
                                <Input id="publication-provider" value={publicationProvider} onChange={(e) => setPublicationProvider(e.target.value)} placeholder="github" />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="publication-repository-id" className="text-sm font-medium">Publication repository URL identity</label>
                                <Input id="publication-repository-id" value={publicationRepositoryID} onChange={(e) => setPublicationRepositoryID(e.target.value)} placeholder="github.com/org/repo" />
                                <p className="text-xs text-muted-foreground">Use the normalized credential-free URL identity, not a GitHub node ID.</p>
                              </div>
                            </div>
                            <div className="grid gap-4 md:grid-cols-2">
                              <div className="space-y-2">
                                <label htmlFor="push-branch" className="text-sm font-medium">Publication branch</label>
                                <Input id="push-branch" value={pushBranch} onChange={(e) => setPushBranch(e.target.value)} placeholder="Leave empty for an Orka-owned branch" />
                              </div>
                              <div className="space-y-2">
                                <label htmlFor="pr-base-branch" className="text-sm font-medium">Pull request base branch</label>
                                <Input id="pr-base-branch" value={prBaseBranch} onChange={(e) => setPRBaseBranch(e.target.value)} placeholder="main" required={workspaceIntent === 'write' && createPR} />
                              </div>
                            </div>
                            <div className="flex items-center gap-2">
                              <Switch id="create-pr" checked={createPR} onCheckedChange={setCreatePR} />
                              <label htmlFor="create-pr" className="text-sm font-medium">Reconcile a pull request after verified publication</label>
                            </div>
                            <div className="space-y-4 border-t pt-4">
                              <div>
                                <p className="text-sm font-medium">Clean-room policies</p>
                                <p className="text-xs text-muted-foreground">Bounds enforced on the workspace delta before publication.</p>
                              </div>
                              <div className="grid gap-4 md:grid-cols-3">
                                <div className="space-y-2">
                                  <label htmlFor="expected-remote-sha" className="text-sm font-medium">Expected remote SHA</label>
                                  <Input id="expected-remote-sha" value={expectedRemoteSHA} onChange={(e) => setExpectedRemoteSHA(e.target.value)} placeholder="Empty means branch must be absent" />
                                </div>
                                <div className="space-y-2">
                                  <label htmlFor="max-changed-files" className="text-sm font-medium">Max changed files</label>
                                  <Input id="max-changed-files" type="number" min="1" value={maxChangedFiles} onChange={(e) => setMaxChangedFiles(e.target.value)} />
                                </div>
                                <div className="space-y-2">
                                  <label htmlFor="allowed-paths" className="text-sm font-medium">Allowed paths (globs)</label>
                                  <Input id="allowed-paths" value={allowedPaths} onChange={(e) => setAllowedPaths(e.target.value)} placeholder="src/**, docs/**" />
                                </div>
                              </div>
                              <div className="flex flex-wrap gap-x-6 gap-y-2">
                                <label className="flex items-center gap-2 text-sm">
                                  <Switch checked={denyRepositoryControlPaths} onCheckedChange={setDenyRepositoryControlPaths} aria-label="Deny repository control paths" />
                                  Deny repository control paths
                                </label>
                                <label className="flex items-center gap-2 text-sm">
                                  <Switch checked={rejectBinaryFiles} onCheckedChange={setRejectBinaryFiles} aria-label="Reject binary files" />
                                  Reject binary files
                                </label>
                                <label className="flex items-center gap-2 text-sm">
                                  <Switch checked={rejectSecretLikeContent} onCheckedChange={setRejectSecretLikeContent} aria-label="Reject secret-like content" />
                                  Reject secret-like content
                                </label>
                              </div>
                            </div>
                          </div>
                        )}
                      </div>
                    )}
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
