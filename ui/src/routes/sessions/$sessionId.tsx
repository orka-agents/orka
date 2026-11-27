import { createFileRoute } from '@tanstack/react-router'
import { SessionDetail } from '@/components/sessions/session-detail'

export const Route = createFileRoute('/sessions/$sessionId')({
  validateSearch: (search: Record<string, unknown>): { tab?: string } => ({
    tab: typeof search.tab === 'string' ? search.tab : undefined,
  }),
  component: SessionDetailPage,
})

function SessionDetailPage() {
  const { sessionId } = Route.useParams()
  return <SessionDetail sessionId={sessionId} />
}
