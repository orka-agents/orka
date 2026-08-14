import { Link } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { useProviderList } from '@/hooks/use-providers'

export function ProviderList() {
  const { data, isLoading } = useProviderList()
  const items = data?.items ?? []

  return (
    <div className="space-y-4">
      <PageHeader
        title="Providers"
        description="LLM providers that agents and tasks route through"
        action={
          <Button asChild>
            <Link to="/providers/new">
              <Plus className="mr-2 h-4 w-4" />
              New provider
            </Link>
          </Button>
        }
      />
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Namespace</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Default model</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: 3 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 5 }).map((_, j) => (
                    <TableCell key={j}><Skeleton className="h-4 w-20" /></TableCell>
                  ))}
                </TableRow>
              ))
            ) : items.length === 0 ? (
              <TableRow>
                <TableCell colSpan={5} className="py-8 text-center text-muted-foreground">
                  No providers yet. Create one to route model calls.
                </TableCell>
              </TableRow>
            ) : (
              items.map((provider) => (
                <TableRow key={provider.name}>
                  <TableCell>
                    <Link
                      to="/providers/$providerName"
                      params={{ providerName: provider.name }}
                      className="font-medium hover:underline"
                    >
                      {provider.name}
                    </Link>
                  </TableCell>
                  <TableCell>{provider.namespace ?? '-'}</TableCell>
                  <TableCell><Badge variant="secondary">{provider.type ?? 'unknown'}</Badge></TableCell>
                  <TableCell className="font-mono text-xs">{provider.defaultModel || '-'}</TableCell>
                  <TableCell>
                    {provider.ready ? (
                      <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Ready</Badge>
                    ) : (
                      <Badge className="bg-status-pending-bg text-status-pending" variant="secondary">Not ready</Badge>
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}
