import { useMemo, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useAgentList } from '@/hooks/use-agents'
import { useCreateRepositoryMonitor } from '@/hooks/use-monitors'
import { useSecretNames } from '@/hooks/use-secrets'
import { useUIStore } from '@/stores/ui'

const REVIEW_EVENTS = ['COMMENT', 'APPROVE', 'REQUEST_CHANGES'] as const

type ReviewEvent = (typeof REVIEW_EVENTS)[number]

function splitCommaList(value: string) {
  const seen = new Set<string>()
  const labels: string[] = []

  for (const raw of value.split(',')) {
    const label = raw.trim()
    if (label && !seen.has(label)) {
      seen.add(label)
      labels.push(label)
    }
  }

  return labels
}

function isCredentialFreeGitHubRepositoryURL(value: string) {
  const repoURL = value.trim()
  if (!repoURL) return false

  if (/^git@github\.com:[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+(?:\.git)?$/.test(repoURL)) {
    return true
  }

  if (!repoURL.startsWith('https://')) {
    return false
  }

  try {
    const parsed = new URL(repoURL)
    if (parsed.protocol !== 'https:' || parsed.hostname !== 'github.com') return false
    if (parsed.username || parsed.password || parsed.search || parsed.hash) return false

    const pathParts = parsed.pathname.split('/').filter(Boolean)
    if (pathParts.length !== 2) return false

    return pathParts.every((part) => /^[A-Za-z0-9._-]+(?:\.git)?$/.test(part))
  } catch {
    return false
  }
}

function maybeObject<T extends Record<string, unknown>>(value: T) {
  const hasValue = Object.values(value).some((field) => {
    if (Array.isArray(field)) return field.length > 0
    return field !== undefined && field !== ''
  })

  return hasValue ? value : undefined
}

export function RepositoryMonitorCreateForm() {
  const navigate = useNavigate()
  const namespace = useUIStore((s) => s.namespace)
  const createMonitor = useCreateRepositoryMonitor()
  const { data: agents, isLoading: agentsLoading } = useAgentList()
  const { data: secrets } = useSecretNames()

  const [name, setName] = useState('')
  const [repoURL, setRepoURL] = useState('')
  const [branch, setBranch] = useState('main')
  const [schedule, setSchedule] = useState('*/30 * * * *')
  const [reviewerAgentName, setReviewerAgentName] = useState('')
  const [gitSecretName, setGitSecretName] = useState('')
  const [includeDrafts, setIncludeDrafts] = useState(false)
  const [maxPRsPerRun, setMaxPRsPerRun] = useState('20')
  const [reviewEvent, setReviewEvent] = useState<ReviewEvent>('COMMENT')
  const [staleReviewTTL, setStaleReviewTTL] = useState('')
  const [exactEventEnabled, setExactEventEnabled] = useState(false)
  const [protectedLabels, setProtectedLabels] = useState('')
  const [pauseLabels, setPauseLabels] = useState('')
  const [formError, setFormError] = useState('')

  const claudeAgents = useMemo(
    () => (agents?.items ?? []).filter((agent) => agent.spec.runtime?.type === 'claude'),
    [agents?.items],
  )

  const validate = () => {
    const trimmedName = name.trim()
    const trimmedRepoURL = repoURL.trim()
    const trimmedReviewerAgentName = reviewerAgentName.trim()

    if (!trimmedName) {
      return 'Monitor name is required.'
    }
    if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(trimmedName)) {
      return 'Monitor name must be a Kubernetes DNS label using lowercase letters, numbers, and hyphens.'
    }
    if (!trimmedRepoURL) {
      return 'GitHub repository URL is required.'
    }
    if (!isCredentialFreeGitHubRepositoryURL(trimmedRepoURL)) {
      return 'Repository URL must be a credential-free GitHub repository root, such as https://github.com/org/repo.'
    }
    if (!trimmedReviewerAgentName) {
      return 'Reviewer Agent name is required.'
    }
    if (!/^\d+$/.test(maxPRsPerRun)) {
      return 'Max PRs per run must be a whole number from 1 to 100.'
    }

    const maxPerRun = Number(maxPRsPerRun)
    if (maxPerRun < 1 || maxPerRun > 100) {
      return 'Max PRs per run must be between 1 and 100.'
    }

    return ''
  }

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault()

    const validationError = validate()
    if (validationError) {
      setFormError(validationError)
      toast.error(validationError)
      return
    }

    const policy = maybeObject({
      protectedLabels: splitCommaList(protectedLabels),
      pauseLabels: splitCommaList(pauseLabels),
    })

    const body = {
      name: name.trim(),
      namespace,
      spec: {
        provider: 'github',
        repoURL: repoURL.trim(),
        branch: branch.trim() || 'main',
        schedule: schedule.trim() || undefined,
        gitSecretRef: gitSecretName.trim() ? { name: gitSecretName.trim() } : undefined,
        targets: {
          pullRequests: {
            enabled: true,
            includeDrafts,
            maxPerRun: Number(maxPRsPerRun),
          },
        },
        agents: {
          reviewer: { name: reviewerAgentName.trim() },
        },
        review: {
          event: reviewEvent,
          staleReviewTTL: staleReviewTTL.trim() || undefined,
          exactEventEnabled,
        },
        policy,
      },
    }

    try {
      const monitor = await createMonitor.mutateAsync(body)
      const monitorName = monitor.metadata.name || body.name
      setFormError('')
      toast.success('Repository monitor created')
      navigate({ to: '/monitors/$monitorId', params: { monitorId: monitorName } })
    } catch (error) {
      const message = error instanceof Error ? error.message : 'Unknown error'
      setFormError(message)
      toast.error(`Failed to create repository monitor: ${message}`)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">New Repository Monitor</h1>
          <p className="text-muted-foreground">
            Create GitHub pull request review automation in the <span className="font-medium text-foreground">{namespace}</span> namespace.
          </p>
        </div>
        <Button type="button" variant="outline" onClick={() => navigate({ to: '/monitors' })}>
          Cancel
        </Button>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Monitor setup</CardTitle>
          <CardDescription>
            Orka stores Secret names only. Add GitHub tokens or Claude keys as Kubernetes Secrets before creating the monitor.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form className="space-y-6" onSubmit={handleSubmit} noValidate>
            {formError ? (
              <div className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive" role="alert">
                {formError}
              </div>
            ) : null}

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label htmlFor="monitor-name" className="text-sm font-medium">Monitor name</label>
                <Input
                  id="monitor-name"
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  placeholder="example-app"
                  aria-invalid={formError.includes('Monitor name')}
                />
                <p className="text-xs text-muted-foreground">Lowercase Kubernetes resource name.</p>
              </div>
              <div className="space-y-2">
                <label htmlFor="monitor-branch" className="text-sm font-medium">Branch</label>
                <Input id="monitor-branch" value={branch} onChange={(event) => setBranch(event.target.value)} placeholder="main" />
              </div>
            </div>

            <div className="space-y-2">
              <label htmlFor="monitor-repo-url" className="text-sm font-medium">GitHub repository URL</label>
              <Input
                id="monitor-repo-url"
                value={repoURL}
                onChange={(event) => setRepoURL(event.target.value)}
                placeholder="https://github.com/org/repo"
                aria-invalid={formError.includes('Repository URL') || formError.includes('GitHub repository URL')}
              />
              <p className="text-xs text-muted-foreground">
                Use a repository root URL only. Pull request, issue, branch, file, query, fragment, HTTP, and token-bearing URLs are rejected.
              </p>
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label htmlFor="monitor-reviewer-agent" className="text-sm font-medium">Reviewer Agent name</label>
                <Input
                  id="monitor-reviewer-agent"
                  list="monitor-reviewer-agent-options"
                  value={reviewerAgentName}
                  onChange={(event) => setReviewerAgentName(event.target.value)}
                  placeholder={agentsLoading ? 'Loading agents...' : 'repo-reviewer'}
                  aria-invalid={formError.includes('Reviewer Agent')}
                />
                <datalist id="monitor-reviewer-agent-options">
                  {claudeAgents.map((agent) => (
                    <option key={agent.metadata.name} value={agent.metadata.name} />
                  ))}
                </datalist>
                <p className="text-xs text-muted-foreground">
                  Must be a Claude runtime Agent with an Anthropic credential Secret in this namespace.
                </p>
              </div>
              <div className="space-y-2">
                <label htmlFor="monitor-git-secret" className="text-sm font-medium">Git Secret name</label>
                <Input
                  id="monitor-git-secret"
                  list="monitor-git-secret-options"
                  value={gitSecretName}
                  onChange={(event) => setGitSecretName(event.target.value)}
                  placeholder="Optional, for private repos or rate limits"
                />
                <datalist id="monitor-git-secret-options">
                  {(secrets?.items ?? []).map((secret) => (
                    <option key={secret.name} value={secret.name} />
                  ))}
                </datalist>
                <p className="text-xs text-muted-foreground">Secret must contain token, password, or GITHUB_TOKEN.</p>
              </div>
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label htmlFor="monitor-schedule" className="text-sm font-medium">Schedule</label>
                <Input id="monitor-schedule" value={schedule} onChange={(event) => setSchedule(event.target.value)} placeholder="*/30 * * * *" />
                <p className="text-xs text-muted-foreground">Leave empty for manual runs only.</p>
              </div>
              <div className="space-y-2">
                <label htmlFor="monitor-max-prs" className="text-sm font-medium">Max PRs per run</label>
                <Input id="monitor-max-prs" type="number" min="1" max="100" value={maxPRsPerRun} onChange={(event) => setMaxPRsPerRun(event.target.value)} />
              </div>
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <label className="flex items-center gap-3 rounded-md border p-3 text-sm">
                <input type="checkbox" checked={includeDrafts} onChange={(event) => setIncludeDrafts(event.target.checked)} />
                <span>
                  <span className="font-medium">Include draft pull requests</span>
                  <span className="block text-xs text-muted-foreground">Allow draft PRs to be selected for review.</span>
                </span>
              </label>
              <label className="flex items-center gap-3 rounded-md border p-3 text-sm">
                <input type="checkbox" checked={exactEventEnabled} onChange={(event) => setExactEventEnabled(event.target.checked)} />
                <span>
                  <span className="font-medium">Enable exact event runs</span>
                  <span className="block text-xs text-muted-foreground">Queue exact-head reviews from GitHub pull request webhook events.</span>
                </span>
              </label>
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label id="monitor-review-event-label" className="text-sm font-medium">Review event</label>
                <Select value={reviewEvent} onValueChange={(value) => setReviewEvent(value as ReviewEvent)}>
                  <SelectTrigger aria-labelledby="monitor-review-event-label">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {REVIEW_EVENTS.map((event) => (
                      <SelectItem key={event} value={event}>{event}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <label htmlFor="monitor-stale-ttl" className="text-sm font-medium">Stale review TTL</label>
                <Input id="monitor-stale-ttl" value={staleReviewTTL} onChange={(event) => setStaleReviewTTL(event.target.value)} placeholder="Optional, e.g. 24h" />
              </div>
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label htmlFor="monitor-protected-labels" className="text-sm font-medium">Protected labels</label>
                <Input id="monitor-protected-labels" value={protectedLabels} onChange={(event) => setProtectedLabels(event.target.value)} placeholder="security-sensitive, customer-data" />
                <p className="text-xs text-muted-foreground">Comma-separated labels that block automation according to policy.</p>
              </div>
              <div className="space-y-2">
                <label htmlFor="monitor-pause-labels" className="text-sm font-medium">Pause labels</label>
                <Input id="monitor-pause-labels" value={pauseLabels} onChange={(event) => setPauseLabels(event.target.value)} placeholder="orka:pause" />
                <p className="text-xs text-muted-foreground">Comma-separated labels that pause further automation while present.</p>
              </div>
            </div>

            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => navigate({ to: '/monitors' })}>Cancel</Button>
              <Button type="submit" disabled={createMonitor.isPending}>
                {createMonitor.isPending ? 'Creating...' : 'Create Monitor'}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
