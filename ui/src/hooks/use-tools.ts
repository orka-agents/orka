import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { Tool, ToolListItem, ToolSpec } from '@/schemas/tool'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useToolList() {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['tools', namespace],
    queryFn: () => api.get<ListResponse<ToolListItem>>('/tools', { namespace }),
  })
}

export function useTool(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['tool', name, namespace],
    queryFn: () => api.get<Tool>(`/tools/${name}`, { namespace }),
  })
}

export function useCreateTool() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; namespace?: string; spec: ToolSpec }) =>
      api.post<Tool>('/tools', body),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['tools'] }) },
  })
}

export function useUpdateTool() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ name, spec }: { name: string; spec: ToolSpec }) =>
      api.put<Tool>(`/tools/${name}`, { spec }, { namespace }),
    onSuccess: (_data, { name }) => {
      queryClient.invalidateQueries({ queryKey: ['tools'] })
      queryClient.invalidateQueries({ queryKey: ['tool', name] })
    },
  })
}

export function useDeleteTool() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (name: string) => api.delete<void>(`/tools/${name}`, { namespace }),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['tools'] }) },
  })
}
