import { createFileRoute } from '@tanstack/react-router'
import { AgentGridView } from '@/components/tasks/agent-grid-view'

export const Route = createFileRoute('/live')({
  component: AgentGridView,
})
