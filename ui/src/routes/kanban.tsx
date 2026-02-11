import { createFileRoute } from '@tanstack/react-router'
import { KanbanBoard } from '@/components/tasks/kanban-board'

export const Route = createFileRoute('/kanban')({
  component: KanbanBoard,
})
