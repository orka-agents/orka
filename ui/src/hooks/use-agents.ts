import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { pageParams, pollUnlessForbidden, retryUnlessForbidden, walkAllPages, type ListResponse } from '@/lib/list-api'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { Agent, AgentSystemPrompt } from '@/schemas/agent'

const AGENT_POLL_INTERVAL_MS = 10_000

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
    retry: retryUnlessForbidden,
    // Active task counts and readiness change server-side; poll like the
    // other resource lists so they do not go stale.
    refetchInterval: pollUnlessForbidden(AGENT_POLL_INTERVAL_MS),
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
    queryFn: () => walkAllPages(
      (continueToken) => api.get<ListResponse<Agent>>('/agents', pageParams({ namespace, limit: '100' }, continueToken)),
      { subject: 'agent list' },
    ),
    enabled: Boolean(token) && (options.enabled ?? true),
  })
}

export function useAgent(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['agent', name, namespace],
    queryFn: () => api.get<Agent>(`/agents/${name}`, { namespace }),
    // A forbidden or deleted Agent stays that way; polling it only spams the API.
    refetchInterval: (query) =>
      isForbiddenError(query.state.error) || isNotFoundError(query.state.error) ? false : AGENT_POLL_INTERVAL_MS,
  })
}

/** Reads the ConfigMap key an Agent's system prompt references; null skips the request. */
export function useAgentSystemPrompt(name: string, configMapRef: { name: string; key: string } | null | undefined) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['agent', name, namespace, 'system-prompt', configMapRef?.name, configMapRef?.key],
    queryFn: () => api.get<AgentSystemPrompt>(`/agents/${name}/system-prompt`, { namespace }),
    enabled: Boolean(configMapRef),
    retry: retryUnlessForbidden,
    // The ConfigMap can change without the Agent changing, so poll it like the Agent.
    refetchInterval: (query) =>
      isForbiddenError(query.state.error) || isNotFoundError(query.state.error) ? false : AGENT_POLL_INTERVAL_MS,
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
