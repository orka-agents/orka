import {
  useInfiniteQuery,
  useQuery,
  useMutation,
  useQueryClient,
  type InfiniteData,
} from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { ExecutionEvent, Task, TaskEventsResponse } from '@/schemas/task'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

const maxTaskListPages = 1000

interface TaskListPaginationState {
  nextCursor?: string
  error?: TaskListPaginationError
}

class TaskListPaginationError extends Error {}

function fetchTaskListPage(namespace: string, limit: string, cursor?: string) {
  const params: Record<string, string> = { namespace, limit, paginate: 'true' }
  if (cursor) params.continue = cursor
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

function getTaskListPaginationState(
  lastPage: ListResponse<Task>,
  pages: ListResponse<Task>[],
  lastPageParam: string | undefined,
  pageParams: Array<string | undefined>,
): TaskListPaginationState {
  const nextCursor = lastPage.metadata?.continue
  if (!nextCursor) return {}
  if (pages.length >= maxTaskListPages) {
    return {
      error: new TaskListPaginationError(
        `Task list pagination page limit (${maxTaskListPages}) reached`,
      ),
    }
  }
  if (nextCursor === lastPageParam) {
    return {
      error: new TaskListPaginationError(
        'Task list pagination continuation did not advance',
      ),
    }
  }
  if (pageParams.includes(nextCursor)) {
    return {
      error: new TaskListPaginationError(
        'Task list pagination continuation cycle detected',
      ),
    }
  }
  return { nextCursor }
}

export function useTaskList(limit = '25', refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  const query = useInfiniteQuery<
    ListResponse<Task>,
    Error,
    InfiniteData<ListResponse<Task>, string | undefined>,
    readonly ['tasks', string, string],
    string | undefined
  >({
    queryKey: ['tasks', namespace, limit],
    queryFn: ({ pageParam }) => fetchTaskListPage(namespace, limit, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage, pages, lastPageParam, pageParams) =>
      getTaskListPaginationState(
        lastPage,
        pages,
        lastPageParam,
        pageParams,
      ).nextCursor,
    refetchInterval,
  })

  const paginationData = query.data
  const lastPage = paginationData?.pages[paginationData.pages.length - 1]
  const paginationError = paginationData && lastPage
    ? getTaskListPaginationState(
        lastPage,
        paginationData.pages,
        paginationData.pageParams[paginationData.pageParams.length - 1],
        paginationData.pageParams,
      ).error
    : undefined

  return {
    ...query,
    data: paginationData
      ? {
          items: flattenUniqueTasks(paginationData.pages, namespace),
          metadata: lastPage?.metadata ?? {},
        }
      : undefined,
    paginationError,
  }
}

export function useTaskListAll(pageLimit = '100', refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['tasks', 'all', namespace, pageLimit],
    queryFn: async () => {
      const pages: ListResponse<Task>[] = []
      const pageParams: Array<string | undefined> = []
      let metadata: ListResponse<Task>['metadata'] = {}
      let cursor: string | undefined

      for (;;) {
        const page = await fetchTaskListPage(namespace, pageLimit, cursor)
        pages.push(page)
        pageParams.push(cursor)
        metadata = page.metadata ?? {}

        const pagination = getTaskListPaginationState(
          page,
          pages,
          cursor,
          pageParams,
        )
        if (pagination.error) throw pagination.error
        if (!pagination.nextCursor) {
          return { items: flattenUniqueTasks(pages, namespace), metadata }
        }

        cursor = pagination.nextCursor
      }
    },
    retry: (failureCount, error) =>
      !(error instanceof TaskListPaginationError) && failureCount < 1,
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
