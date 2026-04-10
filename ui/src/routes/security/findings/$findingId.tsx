import { createFileRoute } from '@tanstack/react-router'
import { FindingDetail } from '@/components/security/finding-detail'

export const Route = createFileRoute('/security/findings/$findingId')({
  component: SecurityFindingPage,
})

function SecurityFindingPage() {
  const { findingId } = Route.useParams()
  return <FindingDetail findingId={findingId} />
}
