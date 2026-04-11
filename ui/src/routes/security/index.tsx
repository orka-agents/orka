import { createFileRoute } from '@tanstack/react-router'
import { RepositoryList } from '@/components/security/repository-list'

export const Route = createFileRoute('/security/')({
  component: RepositoryList,
})
