import { Link } from '@tanstack/react-router'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Plus, Trash2 } from 'lucide-react'
import { PageHeader } from '@/components/layout/page-header'
import { TaskStatusBadge } from './task-status-badge'
import { useTaskList, useDeleteTask } from '@/hooks/use-tasks'
import type { Task } from '@/schemas/task'

function timeAgo(ts?: string): string {
  if (!ts) return '-'
  const s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

export function TaskList() {
  const {
    data,
    isLoading,
    isLoadingError,
    isFetching,
    hasNextPage,
    fetchNextPage,
    isFetchingNextPage,
    isFetchNextPageError,
    refetch,
  } = useTaskList()
  const deleteTask = useDeleteTask()

  return (
    <div className="space-y-4">
      <PageHeader
        title="Tasks"
        description="Manage your task execution"
        action={
          <Link to="/tasks/new">
            <Button>
              <Plus className="mr-2 h-4 w-4" />
              New Task
            </Button>
          </Link>
        }
      />
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Phase</TableHead>
              <TableHead>Namespace</TableHead>
              <TableHead>Age</TableHead>
              <TableHead className="w-12"></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: 5 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 6 }).map((_, j) => (
                    <TableCell key={j}><Skeleton className="h-4 w-20" /></TableCell>
                  ))}
                </TableRow>
              ))
            ) : isLoadingError ? (
              <TableRow>
                <TableCell colSpan={6} className="py-8 text-center">
                  <div className="flex flex-col items-center gap-3">
                    <p
                      role="alert"
                      aria-label="Failed to load tasks"
                      className="text-sm text-destructive"
                    >
                      Failed to load tasks.
                    </p>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => void refetch()}
                      disabled={isFetching}
                    >
                      {isFetching ? 'Retrying…' : 'Retry loading tasks'}
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ) : (data?.items ?? []).length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} className="text-center text-muted-foreground py-8">
                  No tasks found. Create one to get started.
                </TableCell>
              </TableRow>
            ) : (
              (data?.items ?? []).map((task: Task) => (
                <TableRow key={task.metadata.uid || task.metadata.name} className="cursor-pointer">
                  <TableCell>
                    <Link to="/tasks/$taskId" params={{ taskId: task.metadata.name }} className="font-mono text-sm font-medium hover:underline">
                      {task.metadata.name}
                    </Link>
                  </TableCell>
                  <TableCell className="capitalize">{task.spec.type}</TableCell>
                  <TableCell><TaskStatusBadge phase={task.status?.phase} /></TableCell>
                  <TableCell>{task.metadata.namespace}</TableCell>
                  <TableCell>{timeAgo(task.metadata.creationTimestamp)}</TableCell>
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={`Delete task ${task.metadata.name}`}
                      onClick={(e) => {
                        e.preventDefault()
                        e.stopPropagation()
                        deleteTask.mutate(task.metadata.name)
                      }}
                    >
                      <Trash2 className="h-4 w-4 text-muted-foreground" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
        {(hasNextPage || isFetchNextPageError) && (
          <div className="flex flex-col items-center justify-between gap-3 border-t p-3 sm:flex-row">
            {isFetchNextPageError ? (
              <p
                role="alert"
                aria-label="Failed to load more tasks"
                className="text-sm text-destructive"
              >
                Failed to load more tasks. Try again.
              </p>
            ) : null}
            <Button
              variant="outline"
              size="sm"
              className="sm:ml-auto"
              onClick={() => void fetchNextPage()}
              disabled={isFetching}
            >
              {isFetchingNextPage
                ? 'Loading more tasks…'
                : isFetching
                  ? 'Refreshing tasks…'
                : isFetchNextPageError
                  ? 'Retry loading more tasks'
                  : 'Load more tasks'}
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}
