import { Link } from '@tanstack/react-router'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { useToolList } from '@/hooks/use-tools'

export function ToolList() {
  const { data, isLoading } = useToolList()

  return (
    <div className="space-y-4">
      <PageHeader title="Tools" description="Available tools for AI agents" />
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Namespace</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Description</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: 5 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 5 }).map((_, j) => (
                    <TableCell key={j}><Skeleton className="h-4 w-20" /></TableCell>
                  ))}
                </TableRow>
              ))
            ) : (data?.items ?? []).length === 0 ? (
              <TableRow>
                <TableCell colSpan={5} className="text-center text-muted-foreground py-8">
                  No tools found.
                </TableCell>
              </TableRow>
            ) : (
              (data?.items ?? []).map((tool) => (
                <TableRow key={tool.name}>
                  <TableCell>
                    <Link to="/tools/$toolName" params={{ toolName: tool.name }} className="font-medium hover:underline">
                      {tool.name}
                    </Link>
                  </TableCell>
                  <TableCell>{tool.namespace ?? '-'}</TableCell>
                  <TableCell>
                    <Badge variant={tool.builtin ? 'default' : 'secondary'}>
                      {tool.builtin ? 'Built-in' : 'Custom'}
                    </Badge>
                  </TableCell>
                  <TableCell className="max-w-xs truncate">{tool.description}</TableCell>
                  <TableCell>
                    {tool.builtin ? (
                      <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Available</Badge>
                    ) : tool.available ? (
                      <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Available</Badge>
                    ) : (
                      <Badge className="bg-status-failed-bg text-status-failed" variant="secondary">Unavailable</Badge>
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
