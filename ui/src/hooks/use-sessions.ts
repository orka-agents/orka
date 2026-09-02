import { useInfiniteQuery, useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, isForbiddenError } from '@/lib/api-client'
import { useAuthStore } from '@/stores/auth'

// Client errors (403/404/400) do not clear on retry; only transient failures
// do. 408 (request timeout) and 429 (throttled) are client statuses that an
// ingress or API throttle returns transiently, so they keep retrying.
const transientClientStatuses = new Set([408, 429])
export const retryUnlessClientError = (failureCount: number, error: unknown) =>
  failureCount < 3 &&
  !(error instanceof ApiError && error.status >= 400 && error.status < 500 && !transientClientStatuses.has(error.status))
import { useUIStore } from '@/stores/ui'
import type { Session, SessionListItem } from '@/schemas/session'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
  truncated?: boolean
}

export function useSessionList(limit = '25') {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['sessions', namespace, limit],
    queryFn: () => api.get<ListResponse<SessionListItem>>('/sessions', { namespace, limit }),
    enabled: Boolean(token),
    retry: retryUnlessClientError,
    // A 403 will not clear on its own; polling it just spams the audit log.
    refetchInterval: (query) => (isForbiddenError(query.state.error) ? false : 15000),
  })
}

// Page-by-page session listing for the Sessions view; later pages follow
// metadata.continue on demand instead of stopping at the first page.
export function useSessionListPages(limit = '25', refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useInfiniteQuery({
    queryKey: ['sessions', 'pages', namespace, limit],
    queryFn: ({ pageParam }) => {
      const params: Record<string, string> = { namespace, limit }
      if (pageParam) params.continue = pageParam
      return api.get<ListResponse<SessionListItem>>('/sessions', params)
    },
    initialPageParam: '',
    getNextPageParam: (lastPage) => lastPage.metadata?.continue || undefined,
    enabled: Boolean(token),
    retry: retryUnlessClientError,
    refetchInterval: (query) => (isForbiddenError(query.state.error) ? false : refetchInterval),
  })
}

// The walk is bounded like the task-list walk (see maxListWalkPages there):
// unbounded history on a polling interval grows without limit.
export const maxSessionWalkPages = 20

export function useSessionListAll(pageLimit = '100', refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['sessions', 'all', namespace, pageLimit],
    enabled: Boolean(token),
    queryFn: async () => {
      const items: SessionListItem[] = []
      const seen = new Set<string>()
      let metadata: ListResponse<SessionListItem>['metadata'] = {}
      let continueToken: string | undefined
      let pages = 0
      do {
        const params: Record<string, string> = { namespace, limit: pageLimit }
        if (continueToken) params.continue = continueToken
        const page = await api.get<ListResponse<SessionListItem>>('/sessions', params)
        items.push(...page.items)
        metadata = page.metadata ?? {}
        const next = metadata.continue
        if (next && seen.has(next)) throw new Error('session list pagination repeated continuation cursor')
        if (next) seen.add(next)
        continueToken = next || undefined
        pages += 1
      } while (continueToken && pages < maxSessionWalkPages)
      return { items, metadata, truncated: Boolean(continueToken) }
    },
    retry: retryUnlessClientError,
    refetchInterval: (query) => (isForbiddenError(query.state.error) ? false : refetchInterval),
  })
}

export function useSession(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['session', id, namespace],
    queryFn: () => api.get<Session>(`/sessions/${id}`, { namespace }),
    retry: retryUnlessClientError,
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
