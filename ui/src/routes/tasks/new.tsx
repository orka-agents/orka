import { createFileRoute } from '@tanstack/react-router'
import { TaskCreateForm } from '@/components/tasks/task-create-form'

export const Route = createFileRoute('/tasks/new')({
  component: TaskCreateForm,
})
