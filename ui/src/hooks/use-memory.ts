import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { Memory, MemoryFilter, MemoryProposal, MemoryProposalFilter } from '@/schemas/memory'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

// The memory and proposal stores are optional backend capabilities: every
// endpoint returns 501 until the store is configured. Queries stop retrying
// and polling on 501 so pages can render a stable "not enabled" state.
function memoryStoreRetry(failureCount: number, error: Error) {
  return !(error instanceof ApiError && error.status === 501) && failureCount < 3
}

export function isMemoryStoreUnavailable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 501
}

function memoryFilterParams(namespace: string, filter: MemoryFilter): Record<string, string> {
  const params: Record<string, string> = { namespace }
  if (filter.query) params.query = filter.query
  if (filter.sessionName) params.sessionName = filter.sessionName
  if (filter.agentName) params.agentName = filter.agentName
  if (filter.taskName) params.taskName = filter.taskName
  if (filter.parentTask) params.parentTask = filter.parentTask
  if (filter.source) params.source = filter.source
  if (filter.tags?.length) params.tags = filter.tags.join(',')
  if (filter.includeDisabled) params.includeDisabled = 'true'
  if (filter.includeDeleted) params.includeDeleted = 'true'
  if (filter.limit) params.limit = String(filter.limit)
  return params
}

export function useMemoryList(filter: MemoryFilter = {}, refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['memories', namespace, filter],
    queryFn: () => api.get<ListResponse<Memory>>('/memories', memoryFilterParams(namespace, filter)),
    retry: memoryStoreRetry,
    refetchInterval: (query) =>
      query.state.error instanceof ApiError && query.state.error.status === 501
        ? false
        : refetchInterval,
  })
}

export function useMemory(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['memory', id, namespace],
    queryFn: () => api.get<Memory>(`/memories/${id}`, { namespace }),
    enabled: Boolean(id),
    retry: memoryStoreRetry,
  })
}

export function useCreateMemory() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (body: Partial<Memory>) =>
      api.post<Memory>('/memories', { namespace, ...body }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['memories'] })
    },
  })
}

export function useUpdateMemory() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ id, ...body }: Partial<Memory> & { id: string }) =>
      api.put<Memory>(`/memories/${id}`, body, { namespace }),
    onSuccess: (_data, { id }) => {
      queryClient.invalidateQueries({ queryKey: ['memories'] })
      queryClient.invalidateQueries({ queryKey: ['memory', id] })
    },
  })
}

export function useDeleteMemory() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/memories/${id}`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['memories'] })
    },
  })
}

export function useSetMemoryEnabled() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api.post<void>(`/memories/${id}/${enabled ? 'enable' : 'disable'}`, undefined, { namespace }),
    onSuccess: (_data, { id }) => {
      queryClient.invalidateQueries({ queryKey: ['memories'] })
      queryClient.invalidateQueries({ queryKey: ['memory', id] })
    },
  })
}

function proposalFilterParams(namespace: string, filter: MemoryProposalFilter): Record<string, string> {
  const params: Record<string, string> = { namespace }
  if (filter.query) params.query = filter.query
  if (filter.taskName) params.taskName = filter.taskName
  if (filter.agentName) params.agentName = filter.agentName
  if (filter.type) params.type = filter.type
  if (filter.status) params.status = filter.status
  if (filter.limit) params.limit = String(filter.limit)
  return params
}

export function useMemoryProposalList(
  filter: MemoryProposalFilter = {},
  refetchInterval: number | false = 15000,
) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['memoryProposals', namespace, filter],
    queryFn: () =>
      api.get<ListResponse<MemoryProposal>>('/memory-proposals', proposalFilterParams(namespace, filter)),
    retry: memoryStoreRetry,
    refetchInterval: (query) =>
      query.state.error instanceof ApiError && query.state.error.status === 501
        ? false
        : refetchInterval,
  })
}

export function useMemoryProposal(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['memoryProposal', id, namespace],
    queryFn: () => api.get<MemoryProposal>(`/memory-proposals/${id}`, { namespace }),
    enabled: Boolean(id),
    retry: memoryStoreRetry,
  })
}

// Reviewing records the decision (accepted/rejected) without creating memory.
export function useReviewMemoryProposal() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ id, status, reviewNote }: { id: string; status: 'accepted' | 'rejected'; reviewNote?: string }) =>
      api.post<void>(`/memory-proposals/${id}/review`, { status, reviewNote }, { namespace }),
    onSuccess: (_data, { id }) => {
      queryClient.invalidateQueries({ queryKey: ['memoryProposals'] })
      queryClient.invalidateQueries({ queryKey: ['memoryProposal', id] })
    },
  })
}

// Applying an accepted proposal is the explicit step that creates the memory.
export function useApplyMemoryProposal() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ id }: { id: string }) =>
      api.post<Memory>(`/memory-proposals/${id}/apply`, {}, { namespace }),
    onSuccess: (_data, { id }) => {
      queryClient.invalidateQueries({ queryKey: ['memoryProposals'] })
      queryClient.invalidateQueries({ queryKey: ['memoryProposal', id] })
      queryClient.invalidateQueries({ queryKey: ['memories'] })
    },
  })
}

export function useArchiveMemoryProposal() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (id: string) =>
      api.post<void>(`/memory-proposals/${id}/archive`, undefined, { namespace }),
    onSuccess: (_data, id) => {
      queryClient.invalidateQueries({ queryKey: ['memoryProposals'] })
      queryClient.invalidateQueries({ queryKey: ['memoryProposal', id] })
    },
  })
}
