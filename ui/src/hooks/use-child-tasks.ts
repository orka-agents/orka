import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { Task } from '@/schemas/task'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useChildTasks(parentTaskName: string, enabled = true) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['childTasks', parentTaskName, namespace],
    queryFn: () => api.get<ListResponse<Task>>(`/tasks/${parentTaskName}/children`, { namespace }),
    enabled: enabled && !!parentTaskName,
    refetchInterval: 5000,
  })
}
