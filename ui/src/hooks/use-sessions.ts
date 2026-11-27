import { useInfiniteQuery, useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { pageParams, pollUnlessForbidden, maxListWalkPages, walkAllPages, type ListResponse } from '@/lib/list-api'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { Session, SessionListItem } from '@/schemas/session'

const transientClientStatuses = new Set([408, 429])
export const retryUnlessClientError = (failureCount: number, error: unknown) =>
  failureCount < 3 &&
  !(error instanceof ApiError && error.status >= 400 && error.status < 500 && !transientClientStatuses.has(error.status))
export function useSessionListPages(limit = '25', refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useInfiniteQuery({
    queryKey: ['sessions', 'pages', namespace, limit],
    queryFn: ({ pageParam }) =>
      api.get<ListResponse<SessionListItem>>('/sessions', pageParams({ namespace, limit }, pageParam || undefined)),
    initialPageParam: '',
    getNextPageParam: (lastPage) => lastPage.metadata?.continue || undefined,
    enabled: Boolean(token),
    retry: retryUnlessClientError,
    refetchInterval: pollUnlessForbidden(refetchInterval),
  })
}

export function useSessionListAll(pageLimit = '100', refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['sessions', 'all', namespace, pageLimit],
    enabled: Boolean(token),
    queryFn: () => walkAllPages(
      (continueToken) => api.get<ListResponse<SessionListItem>>('/sessions', pageParams({ namespace, limit: pageLimit }, continueToken)),
      { subject: 'session list', maxPages: maxListWalkPages },
    ),
    retry: retryUnlessClientError,
    refetchInterval: pollUnlessForbidden(refetchInterval),
  })
}

export function useSession(id: string, refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['session', id, namespace],
    queryFn: () => api.get<Session>(`/sessions/${id}`, { namespace }),
    retry: retryUnlessClientError,
    refetchInterval: (query) =>
      isForbiddenError(query.state.error) || isNotFoundError(query.state.error) ? false : refetchInterval,
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
