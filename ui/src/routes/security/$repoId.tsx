import { createFileRoute } from '@tanstack/react-router'
import { RepositoryDetail } from '@/components/security/repository-detail'

export const Route = createFileRoute('/security/$repoId')({
  component: SecurityRepositoryPage,
})

function SecurityRepositoryPage() {
  const { repoId } = Route.useParams()
  return <RepositoryDetail repositoryName={repoId} />
}
