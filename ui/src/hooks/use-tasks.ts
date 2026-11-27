import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { isPaginationProtocolError, pageParams, pollUnlessForbidden, retryUnlessForbidden, maxListWalkPages, walkAllPages, type ListResponse } from '@/lib/list-api'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { ExecutionEvent, Task, TaskEventsResponse } from '@/schemas/task'
import { useStickyQueryError } from './use-sticky-query-error'

function fetchTaskListPage(namespace: string, limit: string, continueToken?: string) {
  return api.get<ListResponse<Task>>('/tasks', pageParams({ namespace, limit }, continueToken))
}

export function useTaskListAll(pageLimit = '100', refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  const query = useQuery({
    queryKey: ['tasks', 'all', namespace, pageLimit],
    enabled: Boolean(token),
    queryFn: () => walkAllPages(
      (continueToken) => fetchTaskListPage(namespace, pageLimit, continueToken),
      { subject: 'task list', maxPages: maxListWalkPages },
    ),
    retry: (failureCount, error) =>
      !isForbiddenError(error) && !isPaginationProtocolError(error) && failureCount < 3,
    refetchInterval: pollUnlessForbidden(refetchInterval),
  })
  const error = useStickyQueryError(query)
  return { ...query, error, isLoading: query.isLoading && !error }
}

export function useTask(id: string, refetchInterval: number | false = 5000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['task', id, namespace],
    queryFn: () => api.get<Task>(`/tasks/${id}`, { namespace }),
    retry: (failureCount, error) =>
      !isForbiddenError(error) && !isNotFoundError(error) && failureCount < 3,
    refetchInterval: (query) => {
      if (isForbiddenError(query.state.error)) return false
      if (isNotFoundError(query.state.error) && query.state.data !== undefined) return false
      return refetchInterval
    },
  })
}

export function useTaskResult(id: string, enabled = false, taskUID?: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskResult', id, namespace, taskUID ?? ''],
    queryFn: () =>
      api.get<{ result: string }>(`/tasks/${id}/result`, { namespace }),
    enabled: enabled && Boolean(id),
  })
}

export function useTaskChildren(id: string, enabled = true, taskUID?: string, refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskChildren', id, namespace, taskUID ?? ''],
    queryFn: () => api.get<ListResponse<Task>>(`/tasks/${id}/children`, { namespace }),
    enabled: enabled && Boolean(id),
    retry: retryUnlessForbidden,
    refetchInterval: pollUnlessForbidden(refetchInterval),
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

export interface TaskCancellation {
  namespace: string
  name: string
  phase: string
  requested: boolean
  message: string
}

export function useCancelTask() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ id, uid, reason }: { id: string; uid?: string; reason?: string }) =>
      api.post<TaskCancellation>(`/tasks/${id}/cancel`, { ...(uid && { uid }), ...(reason && { reason }) }, { namespace }),
    onSuccess: (_result, { id }) => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
      queryClient.invalidateQueries({ queryKey: ['task', id, namespace] })
    },
  })
}

const taskEventsPageLimit = '1000'

async function fetchTaskEvents(
  id: string,
  namespace: string,
  previous?: TaskEventsResponse,
): Promise<TaskEventsResponse> {
  let afterSeq = previous?.latestSeq ?? 0
  let targetLatestSeq: number | undefined
  let response: TaskEventsResponse | undefined
  let events: ExecutionEvent[] = [...(previous?.events ?? [])]
  let streamRestarted = false

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
    if (previous && pageResponse.latestSeq < afterSeq) {
      events = []
      afterSeq = 0
      targetLatestSeq = undefined
      response = undefined
      streamRestarted = true
      continue
    }
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

  const latestFetchedSeq = events.reduce(
    (latest, event) => Math.max(latest, event.seq),
    streamRestarted ? 0 : (previous?.latestSeq ?? 0),
  )

  const fetchedThroughServerLatest = response
    ? response.events.length === 0 || latestFetchedSeq >= response.latestSeq
    : false

  return {
    namespace: response?.namespace ?? previous?.namespace ?? namespace,
    streamType: response?.streamType ?? previous?.streamType ?? 'task',
    streamID: response?.streamID ?? previous?.streamID ?? id,
    afterSeq: previous?.afterSeq ?? 0,
    latestSeq: fetchedThroughServerLatest
      ? Math.max(response?.latestSeq ?? 0, latestFetchedSeq)
      : latestFetchedSeq,
    events,
  }
}

export function useTaskEvents(
  id: string,
  refetchInterval: number | false = 5000,
  taskUID?: string,
  enabled = true,
) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  const queryKey = ['taskEvents', id, namespace, taskUID ?? ''] as const
  return useQuery({
    queryKey,
    queryFn: () =>
      fetchTaskEvents(
        id,
        namespace,
        queryClient.getQueryData<TaskEventsResponse>(queryKey),
      ),
    enabled: enabled && Boolean(id),
    retry: (failureCount, error) =>
      !(error instanceof ApiError && (error.status === 501 || error.status === 404 || error.status === 403)) &&
      failureCount < 3,
    refetchInterval: (query) => {
      const error = query.state.error
      if (!(error instanceof ApiError)) return refetchInterval
      if (error.status === 501 || error.status === 403) return false
      if (error.status === 404 && query.state.data !== undefined) return false
      return refetchInterval
    },
  })
}
