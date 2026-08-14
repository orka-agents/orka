import { createFileRoute } from '@tanstack/react-router'
import { ProviderDetail } from '@/components/providers/provider-detail'

export const Route = createFileRoute('/providers/$providerName')({
  component: ProviderDetailPage,
})

function ProviderDetailPage() {
  const { providerName } = Route.useParams()
  return <ProviderDetail providerName={providerName} />
}
