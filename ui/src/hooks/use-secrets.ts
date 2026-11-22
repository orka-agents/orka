import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { retryUnlessForbidden } from '@/lib/list-api'
import { useUIStore } from '@/stores/ui'

interface SecretName {
  name: string
  namespace: string
  type: string
}

export function useSecretNames() {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['secrets', namespace],
    queryFn: () => api.get<{ items: SecretName[] }>('/secrets', { namespace }),
    retry: retryUnlessForbidden,
  })
}
