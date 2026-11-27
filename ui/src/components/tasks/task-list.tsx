import { useMemo, useRef, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { ListAccessError } from '@/components/ui/list-access-error'
import { isForbiddenError } from '@/lib/api-client'
import { Plus, Trash2 } from 'lucide-react'
import { PageHeader } from '@/components/layout/page-header'
import { TaskStatusBadge } from './task-status-badge'
import { useTaskListAll, useDeleteTask } from '@/hooks/use-tasks'
import type { Task } from '@/schemas/task'
import { taskTypeLabel } from '@/lib/task-status'
import { compareTasksNewestFirst, matchesTaskFilter } from '@/lib/task-filter'
import { timeAgo } from '@/lib/time'
import { actionErrorMessage } from './action-error'
import { isTaskDeleting } from './task-state'
import { TaskStateBadges } from './task-state-badges'

const rowsPerReveal = 25

export function TaskList() {
  const { data, isLoading, error: listError } = useTaskListAll('100', 10000)
  const error = listError && (isForbiddenError(listError) || !data) ? listError : null
  const deleteTask = useDeleteTask()
  const navigate = useNavigate()
  const [filter, setFilter] = useState('')
  const [shownCount, setShownCount] = useState(rowsPerReveal)
  const [pendingDelete, setPendingDelete] = useState<Task | null>(null)
  const deletingRef = useRef(false)

  const tasks = useMemo(
    () => (error ? [] : [...(data?.items ?? [])].sort(compareTasksNewestFirst)),
    [data, error],
  )
  const matching = useMemo(() => tasks.filter((task) => matchesTaskFilter(task, filter)), [tasks, filter])
  const visible = matching.slice(0, shownCount)
  const truncated = !error && Boolean(data?.truncated)

  async function confirmDelete() {
    if (!pendingDelete || deletingRef.current) return
    deletingRef.current = true
    const name = pendingDelete.metadata.name
    try {
      await deleteTask.mutateAsync(name)
      toast.success(`Task ${name} deleted`)
    } catch (err) {
      toast.error(actionErrorMessage('Failed to delete task', err))
    } finally {
      deletingRef.current = false
      setPendingDelete(null)
    }
  }

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
      <div className="flex flex-wrap items-center gap-2">
        <Input
          aria-label="Filter tasks"
          placeholder="Filter by name, type, or phase"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="h-9 max-w-sm"
        />
        {tasks.length > 0 && (
          <span className="text-xs text-muted-foreground">
            {filter ? `${matching.length} of ${tasks.length} loaded` : `${tasks.length} loaded`}
            {truncated ? ' · more available' : ''}
          </span>
        )}
      </div>
      {truncated && (
        <p className="text-sm text-muted-foreground" role="status">
          Only the first {tasks.length.toLocaleString()} tasks in resource-key order were loaded, so newer tasks may be missing. More tasks exist.
        </p>
      )}
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead className="hidden sm:table-cell">Type</TableHead>
              <TableHead>Phase</TableHead>
              <TableHead className="hidden md:table-cell">Namespace</TableHead>
              <TableHead className="hidden sm:table-cell">Age</TableHead>
              <TableHead className="w-12"><span className="sr-only">Actions</span></TableHead>
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
            ) : error ? (
              <TableRow>
                <TableCell colSpan={6} className="p-0">
                  <ListAccessError error={error} resource="tasks" />
                </TableCell>
              </TableRow>
            ) : tasks.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} className="text-center text-muted-foreground py-8">
                  No tasks found. Create one to get started.
                </TableCell>
              </TableRow>
            ) : visible.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} className="text-center text-muted-foreground py-8">
                  No loaded tasks match "{filter.trim()}".
                </TableCell>
              </TableRow>
            ) : (
              visible.map((task: Task) => (
                <TableRow
                  key={task.metadata.uid || task.metadata.name}
                  className="cursor-pointer"
                  onClick={(e) => {
                    if ((e.target as HTMLElement).closest('a, button')) return
                    navigate({ to: '/tasks/$taskId', params: { taskId: task.metadata.name } })
                  }}
                >
                  <TableCell className="w-full max-w-0">
                    <Link
                      to="/tasks/$taskId"
                      params={{ taskId: task.metadata.name }}
                      title={task.metadata.name}
                      className="block truncate font-mono text-sm font-medium hover:underline"
                    >
                      {task.metadata.name}
                    </Link>
                  </TableCell>
                  <TableCell className="hidden sm:table-cell">{taskTypeLabel(task.spec.type)}</TableCell>
                  <TableCell>
                    <span className="flex flex-wrap items-center gap-1 lg:flex-nowrap">
                      <TaskStatusBadge phase={task.status?.phase} />
                      <TaskStateBadges task={task} />
                    </span>
                  </TableCell>
                  <TableCell className="hidden md:table-cell">{task.metadata.namespace}</TableCell>
                  <TableCell className="hidden sm:table-cell">{timeAgo(task.metadata.creationTimestamp, { compact: true })}</TableCell>
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={`Delete task ${task.metadata.name}`}
                      title={isTaskDeleting(task) ? 'Deletion in progress' : 'Delete task'}
                      disabled={isTaskDeleting(task)}
                      onClick={(e) => {
                        e.preventDefault()
                        e.stopPropagation()
                        setPendingDelete(task)
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
      </div>
      {matching.length > visible.length && (
        <div className="flex justify-center">
          <Button variant="outline" onClick={() => setShownCount((count) => count + rowsPerReveal)}>
            Show more ({matching.length - visible.length} remaining)
          </Button>
        </div>
      )}
      <Dialog open={pendingDelete !== null} onOpenChange={(open) => { if (!open && !deleteTask.isPending) setPendingDelete(null) }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete task?</DialogTitle>
            <DialogDescription>
              <span className="font-mono">{pendingDelete?.metadata.name}</span> will be deleted. A running task is cancelled.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPendingDelete(null)} disabled={deleteTask.isPending}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={confirmDelete} disabled={deleteTask.isPending}>
              {deleteTask.isPending ? 'Deleting…' : 'Delete'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
