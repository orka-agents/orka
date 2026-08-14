import { Link } from '@tanstack/react-router'

import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { TaskStatusBadge } from '@/components/tasks/task-status-badge'
import { useTaskChildren } from '@/hooks/use-tasks'

// Child tasks resolved through GET /tasks/:id/children — richer than the
// status.childTasks summary (full spec type + live phase per child).
export function TaskChildrenTable({ taskId }: { taskId: string }) {
  const { data, isLoading } = useTaskChildren(taskId)
  const children = data?.items ?? []

  if (isLoading) return <Skeleton className="h-24 w-full" />
  if (children.length === 0) {
    return <p className="text-sm text-muted-foreground">No child tasks resolved by the API.</p>
  }

  return (
    <div className="rounded-md border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Type</TableHead>
            <TableHead>Agent</TableHead>
            <TableHead>Phase</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {children.map((child) => (
            <TableRow key={child.metadata.uid ?? child.metadata.name}>
              <TableCell>
                <Link
                  to="/tasks/$taskId"
                  params={{ taskId: child.metadata.name }}
                  className="font-medium hover:underline"
                >
                  {child.metadata.name}
                </Link>
              </TableCell>
              <TableCell className="font-mono text-xs">{child.spec.type}</TableCell>
              <TableCell>{child.spec.agentRef?.name ?? '—'}</TableCell>
              <TableCell><TaskStatusBadge phase={child.status?.phase} /></TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
