import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { ToolListItem, Tool } from '@/schemas/tool'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useToolList() {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['tools', namespace],
    queryFn: () => api.get<ListResponse<ToolListItem>>('/tools', { namespace }),
    enabled: Boolean(token),
  })
}

export function useToolListAll(pageLimit = '100') {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['tools', 'all', namespace, pageLimit],
    enabled: Boolean(token),
    queryFn: async () => {
      const items: ToolListItem[] = []
      const seen = new Set<string>()
      let continueToken: string | undefined
      do {
        const params: Record<string, string> = { namespace, limit: pageLimit }
        if (continueToken) params.continue = continueToken
        const page = await api.get<ListResponse<ToolListItem>>('/tools', params)
        items.push(...page.items)
        const next = page.metadata?.continue
        if (next && seen.has(next)) throw new Error('tool list pagination repeated continuation cursor')
        if (next) seen.add(next)
        continueToken = next || undefined
      } while (continueToken)
      return { items, metadata: {} } as ListResponse<ToolListItem>
    },
  })
}

export function useTool(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['tool', name, namespace],
    queryFn: () => api.get<Tool>(`/tools/${name}`, { namespace }),
  })
}
