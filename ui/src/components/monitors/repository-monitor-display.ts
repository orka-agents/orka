import type { RepositoryMonitor } from '@/schemas/monitor'

export function repositoryMonitorDisplayName(monitor: RepositoryMonitor) {
  const owner = monitor.spec.owner?.trim()
  const repository = monitor.spec.repository?.trim()
  const parsed = parseGitHubRepoURL(monitor.spec.repoURL)
  const displayOwner = owner || parsed?.owner
  const displayRepository = repository || parsed?.repository || monitor.metadata.name

  if (displayOwner) {
    return `${displayOwner}/${displayRepository}`
  }
  return displayRepository
}

function parseGitHubRepoURL(repoURL: string) {
  const trimmed = repoURL.trim()
  const httpsMatch = trimmed.match(/^https:\/\/github[.]com\/([^/]+)\/([^/?#]+?)(?:[.]git)?\/?$/)
  if (httpsMatch) {
    return { owner: httpsMatch[1], repository: httpsMatch[2] }
  }

  const sshMatch = trimmed.match(/^git@github[.]com:([^/]+)\/(.+?)(?:[.]git)?$/)
  if (sshMatch) {
    return { owner: sshMatch[1], repository: sshMatch[2] }
  }

  return undefined
}
