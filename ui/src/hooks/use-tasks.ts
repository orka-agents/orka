import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { Task, TaskEventsResponse } from '@/schemas/task'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useTaskList(limit = '25', refetchInterval = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['tasks', namespace, limit],
    queryFn: () => api.get<ListResponse<Task>>('/tasks', { namespace, limit }),
    refetchInterval,
  })
}

export function useTask(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['task', id, namespace],
    queryFn: () => api.get<Task>(`/tasks/${id}`, { namespace }),
    refetchInterval: 5000,
  })
}

export function useTaskResult(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskResult', id, namespace],
    queryFn: () =>
      api.get<{ result: string }>(`/tasks/${id}/result`, { namespace }),
    enabled: false,
  })
}

export function useCreateTask() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: Record<string, unknown>) =>
      api.post<Task>('/tasks', body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
  })
}

export function useDeleteTask() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/tasks/${id}`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
  })
}

export function useTaskEvents(id: string, refetchInterval = 5000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskEvents', id, namespace],
    queryFn: () =>
      api.get<TaskEventsResponse>(`/tasks/${id}/events`, { namespace }),
    enabled: Boolean(id),
    refetchInterval,
  })
}
