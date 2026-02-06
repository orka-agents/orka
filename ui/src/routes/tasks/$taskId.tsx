import { createFileRoute } from '@tanstack/react-router'
import { TaskDetail } from '@/components/tasks/task-detail'

export const Route = createFileRoute('/tasks/$taskId')({
  component: TaskDetailPage,
})

function TaskDetailPage() {
  const { taskId } = Route.useParams()
  return <TaskDetail taskId={taskId} />
}
