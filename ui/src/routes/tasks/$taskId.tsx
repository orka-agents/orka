import { createFileRoute } from '@tanstack/react-router'
import { TaskDetail } from '@/components/tasks/task-detail'

export const Route = createFileRoute('/tasks/$taskId')({
  validateSearch: (search: Record<string, unknown>): { tab?: string } => ({
    tab: typeof search.tab === 'string' ? search.tab : undefined,
  }),
  component: TaskDetailPage,
})

function TaskDetailPage() {
  const { taskId } = Route.useParams()
  return <TaskDetail taskId={taskId} />
}
