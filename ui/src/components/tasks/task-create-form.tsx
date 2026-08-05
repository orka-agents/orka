import { useMemo, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import { PageHeader } from '@/components/layout/page-header'
import { useCreateTask } from '@/hooks/use-tasks'
import { useAgentList } from '@/hooks/use-agents'
import { useUIStore } from '@/stores/ui'
import { toast } from 'sonner'
import { workspaceConfigSchema, type WorkspaceIntent } from '@/schemas/task'
import { builtInAgentRuntimeLabel } from '@/lib/agent-runtime'

function optionalRepositoryIdentity(provider: string, id: string) {
  return provider.trim() && id.trim() ? { provider: provider.trim(), id: id.trim() } : undefined
}

function optionalCredentialReference(name: string, key: string) {
  const trimmedName = name.trim()
  if (!trimmedName) return undefined
  const trimmedKey = key.trim()
  return trimmedKey ? { name: trimmedName, key: trimmedKey } : { name: trimmedName }
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

  const [showAdvanced, setShowAdvanced] = useState(false)
  const [priority, setPriority] = useState('')
  const [timeout, setTimeout] = useState('')

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

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault()
    const body: Record<string, unknown> = { name, namespace, type }

    if (type === 'container') {
      body.image = image
      if (command) body.command = command.split(' ')
    } else if (type === 'ai') {
      body.ai = { provider, model, prompt }
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
        toast.error(`${credentialWithoutName.label} Secret is required when a key is set`)
        return
      }
      if (workspaceIntent === 'write' && !gitRepo.trim()) {
        toast.error('Source repository URL is required for write workspaces')
        return
      }
      if (workspaceIntent === 'write' && !publicationCredentialName.trim()) {
        toast.error('Publication write credential Secret is required for write workspaces')
        return
      }
      if (workspaceIntent === 'write' && createPR && !prBaseBranch.trim()) {
        toast.error('Pull request base branch is required when creating a pull request')
        return
      }
      if (workspaceIntent === 'write' && createPR && !forgeCredentialName.trim()) {
        toast.error('Forge credential Secret is required when creating a pull request')
        return
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
        toast.error(workspaceResult.error.issues[0]?.message ?? 'Invalid workspace configuration')
        return
      }
      body.workspace = workspaceResult.data
    }

    if (priority) body.priority = parseInt(priority)
    if (timeout) body.timeout = timeout

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
      <PageHeader title="Create Task" description="Create a new task for execution" />
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
