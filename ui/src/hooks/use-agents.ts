import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { Agent } from '@/schemas/agent'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useAgentList() {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['agents', namespace],
    queryFn: () => api.get<ListResponse<Agent>>('/agents', { namespace }),
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

export function useUpdateAgent() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ name, spec }: { name: string; spec: Record<string, unknown> }) =>
      api.put<Agent>(`/agents/${name}`, { spec }, { namespace }),
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
