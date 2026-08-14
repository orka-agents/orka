import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import type { WhoAmI } from '@/schemas/auth'

// Verified caller identity as the API server sees it. Stable per token, so a
// long stale time keeps the header identity cheap.
export function useWhoAmI() {
  return useQuery({
    queryKey: ['whoami'],
    queryFn: () => api.get<WhoAmI>('/auth/whoami'),
    staleTime: 5 * 60 * 1000,
  })
}
