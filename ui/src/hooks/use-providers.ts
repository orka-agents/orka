import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { providerListResponseSchema, type ProviderListItem } from '@/schemas/provider'
import { useUIStore } from '@/stores/ui'

const PROVIDER_PAGE_LIMIT = '100'

// Providers are a paginated Kubernetes-backed list; the picker must see every
// Provider in the namespace, so all pages are followed like the task hooks do.
export function useProviderList() {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['providers', namespace],
    queryFn: async () => {
      const items: ProviderListItem[] = []
      const seen = new Set<string>()
      let continueToken: string | undefined
      do {
        const params: Record<string, string> = { namespace, limit: PROVIDER_PAGE_LIMIT }
        if (continueToken) params.continue = continueToken
        const page = providerListResponseSchema.parse(await api.get<unknown>('/providers', params))
        items.push(...page.items)
        const next = page.metadata?.continue
        if (next && seen.has(next)) {
          throw new Error('provider list pagination repeated continuation cursor')
        }
        if (next) seen.add(next)
        continueToken = next || undefined
      } while (continueToken)
      return { items, metadata: {} }
    },
    staleTime: 60 * 1000,
  })
}
