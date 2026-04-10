import { createFileRoute } from '@tanstack/react-router'
import { RepositoryCreateForm } from '@/components/security/repository-create-form'

export const Route = createFileRoute('/security/new')({
  component: RepositoryCreateForm,
})
