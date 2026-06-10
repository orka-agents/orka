import { createFileRoute } from '@tanstack/react-router'
import { RepositoryMonitorCreateForm } from '@/components/monitors/repository-monitor-create-form'

export const Route = createFileRoute('/monitors/create/new')({
  component: RepositoryMonitorCreateForm,
})
