import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, isForbiddenError } from '@/lib/api-client'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { Agent } from '@/schemas/agent'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

interface AgentListOptions {
  namespace?: string
  enabled?: boolean
}

export function useAgentList(options: AgentListOptions = {}) {
  const selectedNamespace = useUIStore((s) => s.namespace)
  const namespace = options.namespace ?? selectedNamespace
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['agents', namespace],
    queryFn: () => api.get<ListResponse<Agent>>('/agents', { namespace }),
    enabled: Boolean(token) && (options.enabled ?? true),
    retry: (failureCount, error) => !isForbiddenError(error) && failureCount < 3,
  })
}

// Follows metadata.continue so selectors that must see every Agent in a
// namespace (for example the task creation form) are not capped at one page.
export function useAgentListAll(options: AgentListOptions = {}) {
  const selectedNamespace = useUIStore((s) => s.namespace)
  const namespace = options.namespace ?? selectedNamespace
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['agents', 'all', namespace],
    queryFn: async () => {
      const items: Agent[] = []
      const seen = new Set<string>()
      let continueToken: string | undefined
      do {
        const params: Record<string, string> = { namespace, limit: '100' }
        if (continueToken) params.continue = continueToken
        const page = await api.get<ListResponse<Agent>>('/agents', params)
        items.push(...page.items)
        const next = page.metadata?.continue
        if (next && seen.has(next)) {
          throw new Error('agent list pagination repeated continuation cursor')
        }
        if (next) seen.add(next)
        continueToken = next || undefined
      } while (continueToken)
      return { items, metadata: {} } as ListResponse<Agent>
    },
    enabled: Boolean(token) && (options.enabled ?? true),
  })
}

export function useAgent(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['agent', name, namespace],
    queryFn: () => api.get<Agent>(`/agents/${name}`, { namespace }),
  })
}

export function useCreateAgent() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: Record<string, unknown>) => api.post<Agent>('/agents', body),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['agents'] }) },
  })
}

export function useDeleteAgent() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (name: string) => api.delete<void>(`/agents/${name}`, { namespace }),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['agents'] }) },
  })
}
