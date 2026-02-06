import { createFileRoute } from '@tanstack/react-router'
import { AgentList } from '@/components/agents/agent-list'

export const Route = createFileRoute('/agents/')({
  component: AgentList,
})
