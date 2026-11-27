import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { pageParams, walkAllPages } from '@/lib/list-api'
import { providerListResponseSchema, type ProviderListItem } from '@/schemas/provider'
import { useUIStore } from '@/stores/ui'

const PROVIDER_PAGE_LIMIT = '100'

export function useProviderList() {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['providers', namespace],
    queryFn: () => walkAllPages<ProviderListItem>(
      async (continueToken) => providerListResponseSchema.parse(
        await api.get<unknown>('/providers', pageParams({ namespace, limit: PROVIDER_PAGE_LIMIT }, continueToken)),
      ),
      { subject: 'provider list' },
    ),
    staleTime: 60 * 1000,
  })
}
