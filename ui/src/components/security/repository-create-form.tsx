import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useCreateRepositoryScan } from '@/hooks/use-security'
import { useAgentList } from '@/hooks/use-agents'
import { useSecretNames } from '@/hooks/use-secrets'
import { useUIStore } from '@/stores/ui'

export function RepositoryCreateForm() {
  const navigate = useNavigate()
  const namespace = useUIStore((s) => s.namespace)
  const setNamespace = useUIStore((s) => s.setNamespace)
  const createRepository = useCreateRepositoryScan()
  const { data: agents, isLoading: agentsLoading } = useAgentList()
  const currentAgents = agents?.items ?? []
  const shouldCheckSystemAgents = namespace !== 'orka-system' && currentAgents.length === 0
  const { data: systemAgents } = useAgentList({ namespace: 'orka-system', enabled: shouldCheckSystemAgents })
  const { data: secrets } = useSecretNames()
  const systemAgentCount = systemAgents?.items.length ?? 0
  const noAgentsInNamespace = !agentsLoading && currentAgents.length === 0

  const [name, setName] = useState('')
  const [repoURL, setRepoURL] = useState('')
  const [branch, setBranch] = useState('main')
  const [subPath, setSubPath] = useState('')
  const [schedule, setSchedule] = useState('0 */6 * * *')
  const [historyDays, setHistoryDays] = useState('30')
  const [validationMode, setValidationMode] = useState('light')
  const [analysisAgentRef, setAnalysisAgentRef] = useState('')
  const [patchAgentRef, setPatchAgentRef] = useState('')
  const [gitSecretRef, setGitSecretRef] = useState('')

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault()

    if (!analysisAgentRef) {
      toast.error('Select an analysis agent before registering the repository')
      return
    }

    try {
      const body: Record<string, unknown> = {
        name,
        namespace,
        spec: {
          repoURL,
          branch,
          subPath: subPath || undefined,
          schedule: schedule || undefined,
          historyDays: historyDays ? parseInt(historyDays, 10) : undefined,
          validationMode,
          analysisAgentRef: { name: analysisAgentRef },
          patchAgentRef: patchAgentRef ? { name: patchAgentRef } : undefined,
          gitSecretRef: gitSecretRef ? { name: gitSecretRef } : undefined,
        },
      }
      await createRepository.mutateAsync(body)
      toast.success('Repository registered')
      navigate({ to: '/security' })
    } catch (error) {
      toast.error(`Failed to register repository: ${error instanceof Error ? error.message : 'Unknown error'}`)
    }
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-3xl font-bold tracking-tight">New Security Repository</h1>
        <p className="text-muted-foreground">Register a GitHub repository for threat modeling and code scanning</p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Repository Setup</CardTitle>
        </CardHeader>
        <CardContent>
          <form className="space-y-4" onSubmit={handleSubmit}>
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Name</label>
                <Input value={name} onChange={(event) => setName(event.target.value)} placeholder="payments-api" required />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Branch</label>
                <Input value={branch} onChange={(event) => setBranch(event.target.value)} placeholder="main" />
              </div>
            </div>

            <div className="space-y-2">
              <label className="text-sm font-medium">GitHub Repository URL</label>
              <Input value={repoURL} onChange={(event) => setRepoURL(event.target.value)} placeholder="https://github.com/org/repo" required />
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Analysis Agent</label>
                <Select disabled={agentsLoading || noAgentsInNamespace} value={analysisAgentRef} onValueChange={setAnalysisAgentRef}>
                  <SelectTrigger><SelectValue placeholder={agentsLoading ? 'Loading agents...' : 'Select analysis agent'} /></SelectTrigger>
                  <SelectContent>
                    {currentAgents.map((agent) => (
                      <SelectItem key={agent.metadata.name} value={agent.metadata.name}>{agent.metadata.name}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Patch Agent</label>
                <Select disabled={agentsLoading || noAgentsInNamespace} value={patchAgentRef} onValueChange={setPatchAgentRef}>
                  <SelectTrigger><SelectValue placeholder="Optional patch agent" /></SelectTrigger>
                  <SelectContent>
                    {currentAgents.map((agent) => (
                      <SelectItem key={agent.metadata.name} value={agent.metadata.name}>{agent.metadata.name}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="space-y-2 rounded-md border border-dashed border-border p-3">
              <p className="text-sm text-muted-foreground">
                Showing agents from the <span className="font-medium text-foreground">{namespace}</span> namespace.
              </p>
              {noAgentsInNamespace ? (
                <div className="space-y-2 text-sm text-muted-foreground">
                  <p>No agents are available in this namespace.</p>
                  {systemAgentCount > 0 ? (
                    <div className="flex items-center gap-2">
                      <span>{systemAgentCount} agent(s) are available in <span className="font-medium text-foreground">orka-system</span>.</span>
                      <Button type="button" variant="outline" size="sm" onClick={() => setNamespace('orka-system')}>
                        Switch to orka-system
                      </Button>
                    </div>
                  ) : (
                    <p>Create agents in this namespace or switch namespaces from the header.</p>
                  )}
                </div>
              ) : null}
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Git Secret</label>
                <Select value={gitSecretRef} onValueChange={setGitSecretRef}>
                  <SelectTrigger><SelectValue placeholder="Optional Git credential secret" /></SelectTrigger>
                  <SelectContent>
                    {(secrets?.items ?? []).map((secret) => (
                      <SelectItem key={secret.name} value={secret.name}>{secret.name}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Validation Mode</label>
                <Select value={validationMode} onValueChange={setValidationMode}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="off">Off</SelectItem>
                    <SelectItem value="light">Light</SelectItem>
                    <SelectItem value="full">Full</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Schedule</label>
                <Input value={schedule} onChange={(event) => setSchedule(event.target.value)} placeholder="0 */6 * * *" />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">History Window (days)</label>
                <Input value={historyDays} onChange={(event) => setHistoryDays(event.target.value)} type="number" min={1} />
              </div>
            </div>

            <div className="space-y-2">
              <label className="text-sm font-medium">Sub-path</label>
              <Input value={subPath} onChange={(event) => setSubPath(event.target.value)} placeholder="services/api" />
            </div>

            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => navigate({ to: '/security' })}>Cancel</Button>
              <Button type="submit" disabled={createRepository.isPending || noAgentsInNamespace}>Register Repository</Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
