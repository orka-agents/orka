import { createFileRoute } from '@tanstack/react-router'
import { ProviderList } from '@/components/providers/provider-list'

export const Route = createFileRoute('/providers/')({
  component: ProviderList,
})
