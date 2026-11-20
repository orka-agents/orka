import { createFileRoute } from '@tanstack/react-router'
import { KanbanBoard } from '@/components/tasks/kanban-board'
import { parseKanbanView, type KanbanView } from '@/lib/attention-lanes'

export const Route = createFileRoute('/kanban')({
  validateSearch: (search: Record<string, unknown>): { view?: KanbanView } => ({
    view: search.view === undefined ? undefined : parseKanbanView(search.view),
  }),
  component: KanbanPage,
})

function KanbanPage() {
  const { view } = Route.useSearch()
  return <KanbanBoard view={view ?? 'phase'} />
}
