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
    // Editors are deliberately not allowed to list Secrets; a 403 is final,
    // so the picker switches to a typed name without waiting out retries.
    retry: retryUnlessForbidden,
  })
}
