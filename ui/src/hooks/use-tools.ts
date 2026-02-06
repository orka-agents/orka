import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { ToolListItem, Tool } from '@/schemas/tool'

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
