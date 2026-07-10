import { useInfiniteQuery, useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { ExecutionEvent, Task, TaskEventsResponse } from '@/schemas/task'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

function fetchTaskListPage(namespace: string, limit: string, continueToken?: string) {
  const params: Record<string, string> = { namespace, limit }
  if (continueToken) params.continue = continueToken
  return api.get<ListResponse<Task>>('/tasks', params)
}

function flattenUniqueTasks(pages: ListResponse<Task>[], namespace: string) {
  const items: Task[] = []
  const seenUIDs = new Set<string>()
  const seenNames = new Set<string>()

  for (const page of pages) {
    for (const task of page.items) {
      const uid = task.metadata.uid
      const namespacedName = `${task.metadata.namespace ?? namespace}/${task.metadata.name}`
      if ((uid && seenUIDs.has(uid)) || seenNames.has(namespacedName)) {
        continue
      }
      if (uid) seenUIDs.add(uid)
      seenNames.add(namespacedName)
      items.push(task)
    }
  }

  return items
}

export function useTaskList(limit = '25', refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  return useInfiniteQuery({
    queryKey: ['tasks', namespace, limit],
    queryFn: ({ pageParam }) => fetchTaskListPage(namespace, limit, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage, _pages, lastPageParam, pageParams) => {
      const nextPageParam = lastPage.metadata?.continue
      if (
        !nextPageParam ||
        nextPageParam === lastPageParam ||
        pageParams.includes(nextPageParam)
      ) {
        return undefined
      }
      return nextPageParam
    },
    select: (data) => ({
      items: flattenUniqueTasks(data.pages, namespace),
      metadata: data.pages[data.pages.length - 1]?.metadata ?? {},
    }),
    refetchInterval,
  })
}

export function useTaskListAll(pageLimit = '100', refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['tasks', 'all', namespace, pageLimit],
    queryFn: async () => {
      const items: Task[] = []
      let metadata: ListResponse<Task>['metadata'] = {}
      let continueToken: string | undefined
      do {
        const page = await fetchTaskListPage(namespace, pageLimit, continueToken)
        items.push(...page.items)
        metadata = page.metadata ?? {}
        continueToken = metadata.continue
      } while (continueToken)
      return { items, metadata }
    },
    refetchInterval,
  })
}

export function useTask(id: string, refetchInterval: number | false = 5000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['task', id, namespace],
    queryFn: () => api.get<Task>(`/tasks/${id}`, { namespace }),
    refetchInterval,
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
    enabled: Boolean(id),
    retry: (failureCount, error) =>
      !(error instanceof ApiError && error.status === 501) && failureCount < 3,
    refetchInterval: (query) =>
      query.state.error instanceof ApiError && query.state.error.status === 501
        ? false
        : refetchInterval,
  })
}
