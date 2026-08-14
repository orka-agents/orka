import { createFileRoute } from '@tanstack/react-router'
import { ProviderForm } from '@/components/providers/provider-form'

export const Route = createFileRoute('/providers/new')({
  component: ProviderForm,
})
