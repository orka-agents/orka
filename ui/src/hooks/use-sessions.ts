import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { Session, SessionListItem } from '@/schemas/session'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useSessionList(limit = '25') {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['sessions', namespace, limit],
    queryFn: () => api.get<ListResponse<SessionListItem>>('/sessions', { namespace, limit }),
    refetchInterval: 15000,
  })
}

export function useSession(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['session', id, namespace],
    queryFn: () => api.get<Session>(`/sessions/${id}`, { namespace }),
  })
}

export function useDeleteSession() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/sessions/${id}`, { namespace }),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['sessions'] }) },
  })
}
