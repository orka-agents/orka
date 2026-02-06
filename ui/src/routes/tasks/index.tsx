import { createFileRoute } from '@tanstack/react-router'
import { TaskList } from '@/components/tasks/task-list'

export const Route = createFileRoute('/tasks/')({
  component: TaskList,
})
