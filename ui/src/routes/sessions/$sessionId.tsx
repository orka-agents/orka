import { createFileRoute } from '@tanstack/react-router'
import { SessionDetail } from '@/components/sessions/session-detail'

export const Route = createFileRoute('/sessions/$sessionId')({
  component: SessionDetailPage,
})

function SessionDetailPage() {
  const { sessionId } = Route.useParams()
  return <SessionDetail sessionId={sessionId} />
}
