import { createFileRoute } from '@tanstack/react-router'
import { ProviderForm } from '@/components/providers/provider-form'
import { Skeleton } from '@/components/ui/skeleton'
import { useProvider } from '@/hooks/use-providers'

export const Route = createFileRoute('/providers/$providerName_/edit')({
  component: ProviderEditPage,
})

function ProviderEditPage() {
  const { providerName } = Route.useParams()
  const { data: provider, isLoading } = useProvider(providerName)
  if (isLoading || !provider) return <Skeleton className="h-64 w-full" />
  return <ProviderForm initial={provider} />
}
