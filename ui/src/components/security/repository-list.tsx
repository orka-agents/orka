import { Link } from '@tanstack/react-router'
import { Shield, Plus, RefreshCcw } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { useRepositoryScans, useRunSecurityScan } from '@/hooks/use-security'
import type { RepositoryScan } from '@/schemas/security'

function timeAgo(ts?: string) {
  if (!ts) return 'Never'
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(ts).getTime()) / 1000))
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

export function RepositoryList() {
  const { data, isLoading } = useRepositoryScans()

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Security</h1>
          <p className="text-muted-foreground">Repository security scans, threat models, and findings</p>
        </div>
        <Link to="/security/new">
          <Button><Plus className="mr-2 h-4 w-4" />New Repository</Button>
        </Link>
      </div>

      {isLoading ? (
        <div className="grid gap-4 md:grid-cols-2">
          {Array.from({ length: 4 }).map((_, index) => (
            <Card key={index}>
              <CardHeader><Skeleton className="h-6 w-48" /></CardHeader>
              <CardContent className="space-y-2">
                <Skeleton className="h-4 w-full" />
                <Skeleton className="h-4 w-2/3" />
              </CardContent>
            </Card>
          ))}
        </div>
      ) : (data?.items ?? []).length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            No repositories configured for security scanning yet.
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {(data?.items ?? []).map((repo) => (
            <RepositoryCard key={repo.metadata.name} repo={repo} />
          ))}
        </div>
      )}
    </div>
  )
}

function RepositoryCard({ repo }: { repo: RepositoryScan }) {
  const runScan = useRunSecurityScan(repo.metadata.name)

  return (
    <Card className="transition-colors hover:border-primary/50">
      <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-lg">
            <Shield className="h-5 w-5 text-primary" />
            <Link to="/security/$repoId" params={{ repoId: repo.metadata.name }} className="hover:underline">
              {repo.spec.repository || repo.metadata.name}
            </Link>
          </CardTitle>
          <p className="text-sm text-muted-foreground">{repo.spec.owner}/{repo.spec.repository} · {repo.spec.branch || 'main'}</p>
        </div>
        <Badge variant={repo.status?.phase === 'Ready' ? 'default' : 'secondary'}>
          {repo.status?.phase || 'Pending'}
        </Badge>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="text-sm text-muted-foreground">{repo.spec.repoURL}</div>
        <div className="grid gap-2 text-sm md:grid-cols-2">
          <div>Open findings: <span className="font-medium text-foreground">{repo.status?.findingCounts?.total ?? 0}</span></div>
          <div>Last scan: <span className="font-medium text-foreground">{timeAgo(repo.status?.lastScanAt)}</span></div>
        </div>
        <div className="flex flex-wrap gap-2 text-xs">
          <Badge variant="outline">Critical {repo.status?.findingCounts?.critical ?? 0}</Badge>
          <Badge variant="outline">High {repo.status?.findingCounts?.high ?? 0}</Badge>
          <Badge variant="outline">Medium {repo.status?.findingCounts?.medium ?? 0}</Badge>
          <Badge variant="outline">Low {repo.status?.findingCounts?.low ?? 0}</Badge>
        </div>
        <div className="flex items-center justify-between">
          <Link to="/security/$repoId" params={{ repoId: repo.metadata.name }}>
            <Button variant="outline">Open</Button>
          </Link>
          <Button variant="secondary" onClick={() => runScan.mutate()} disabled={runScan.isPending}>
            <RefreshCcw className="mr-2 h-4 w-4" />
            Scan Now
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
