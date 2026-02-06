import { createFileRoute } from '@tanstack/react-router'
import { SessionList } from '@/components/sessions/session-list'

export const Route = createFileRoute('/sessions/')({
  component: SessionList,
})
