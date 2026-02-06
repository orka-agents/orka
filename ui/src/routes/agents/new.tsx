import { createFileRoute } from '@tanstack/react-router'
import { AgentCreateForm } from '@/components/agents/agent-create-form'

export const Route = createFileRoute('/agents/new')({
  component: AgentCreateForm,
})
