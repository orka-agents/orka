import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { ExecutionEvent, Task, TaskEventsResponse } from '@/schemas/task'

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

const taskEventsPageLimit = '1000'

export async function fetchTaskEvents(
  id: string,
  namespace: string,
): Promise<TaskEventsResponse> {
  let afterSeq = 0
  let targetLatestSeq: number | undefined
  let response: TaskEventsResponse | undefined
  const events: ExecutionEvent[] = []

  let keepFetching = true
  while (keepFetching) {
    const params: Record<string, string> = {
      namespace,
      limit: taskEventsPageLimit,
    }
    if (afterSeq > 0) {
      params.after = String(afterSeq)
    }

    const pageResponse = await api.get<TaskEventsResponse>(
      `/tasks/${id}/events`,
      params,
    )
    response = pageResponse
    targetLatestSeq ??= pageResponse.latestSeq
    events.push(...pageResponse.events)

    const lastEvent = pageResponse.events[pageResponse.events.length - 1]
    const lastSeq = lastEvent?.seq ?? afterSeq
    if (lastSeq <= afterSeq) {
      keepFetching = false
      continue
    }
    afterSeq = lastSeq
    if (afterSeq >= targetLatestSeq || pageResponse.events.length === 0) {
      keepFetching = false
    }
  }

  return {
    namespace: response?.namespace ?? namespace,
    streamType: response?.streamType ?? 'task',
    streamID: response?.streamID ?? id,
    afterSeq: 0,
    latestSeq: targetLatestSeq ?? response?.latestSeq ?? 0,
    events,
  }
}

export function useTaskEvents(id: string, refetchInterval = 5000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskEvents', id, namespace],
    queryFn: () => fetchTaskEvents(id, namespace),
    enabled: Boolean(id),
    refetchInterval,
  })
}
