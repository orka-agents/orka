import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { agentRuntimeListSchema, runtimePoolListSchema } from '@/schemas/runtime'
import type { AgentRuntime, SubstrateActorPool, SubstrateActorPoolSpec } from '@/schemas/runtime'
import { useUIStore } from '@/stores/ui'

interface RuntimeListPage<T> {
  items: T[]
  metadata?: { continue?: string; remainingItemCount?: number }
}

interface RuntimeListPageSchema<T> {
  parse(value: unknown): RuntimeListPage<T>
}

async function fetchAllRuntimePages<T>(
  path: '/runtime-pools' | '/agent-runtimes',
  namespace: string,
  schema: RuntimeListPageSchema<T>,
): Promise<RuntimeListPage<T>> {
  const items: T[] = []
  const seenCursors = new Set<string>()
  let metadata: RuntimeListPage<T>['metadata']
  let cursor: string | undefined

  do {
    const params: Record<string, string> = { namespace, limit: '100' }
    if (cursor) params.continue = cursor

    const page = schema.parse(await api.get<unknown>(path, params))
    items.push(...page.items)
    metadata = page.metadata

    const nextCursor = metadata?.continue
    if (!nextCursor) break
    if (seenCursors.has(nextCursor)) {
      throw new Error(`runtime list pagination repeated continuation cursor for ${path}`)
    }
    seenCursors.add(nextCursor)
    cursor = nextCursor
  } while (cursor)

  return { items, metadata }
}

export function useRuntimePools(refetchInterval: number | false = 5000) {
  const namespace = useUIStore((state) => state.namespace)
  return useQuery({
    queryKey: ['runtime-pools', namespace],
    queryFn: () => fetchAllRuntimePages('/runtime-pools', namespace, runtimePoolListSchema),
    refetchInterval,
  })
}

export function useAgentRuntimes(refetchInterval: number | false = 10000) {
  const namespace = useUIStore((state) => state.namespace)
  return useQuery({
    queryKey: ['agent-runtimes', namespace],
    queryFn: () => fetchAllRuntimePages('/agent-runtimes', namespace, agentRuntimeListSchema),
    refetchInterval,
  })
}

// ---- External AgentRuntime registration (create/update/delete) ----

export function useCreateAgentRuntime() {
  const queryClient = useQueryClient()
  return useMutation({
    // POST /agent-runtimes takes a full AgentRuntime manifest; the server
    // resets status/uid/resourceVersion itself.
    mutationFn: (body: Record<string, unknown>) =>
      api.post<AgentRuntime>('/agent-runtimes', body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['agent-runtimes'] })
    },
  })
}

export function useUpdateAgentRuntime() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((state) => state.namespace)
  return useMutation({
    // PUT applies only .spec from the submitted manifest.
    mutationFn: ({ name, body }: { name: string; body: Record<string, unknown> }) =>
      api.put<AgentRuntime>(`/agent-runtimes/${name}`, body, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['agent-runtimes'] })
    },
  })
}

export function useDeleteAgentRuntime() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((state) => state.namespace)
  return useMutation({
    mutationFn: (name: string) => api.delete<void>(`/agent-runtimes/${name}`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['agent-runtimes'] })
    },
  })
}

// ---- SubstrateActorPools ----

interface SubstrateActorPoolListResponse {
  items: SubstrateActorPool[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useSubstrateActorPools(refetchInterval: number | false = 10000) {
  const namespace = useUIStore((state) => state.namespace)
  return useQuery({
    queryKey: ['substrate-actor-pools', namespace],
    queryFn: () =>
      api.get<SubstrateActorPoolListResponse>('/substrate-actor-pools', { namespace }),
    refetchInterval,
  })
}

export function useSubstrateActorPool(name: string) {
  const namespace = useUIStore((state) => state.namespace)
  return useQuery({
    queryKey: ['substrate-actor-pool', name, namespace],
    queryFn: () => api.get<SubstrateActorPool>(`/substrate-actor-pools/${name}`, { namespace }),
    enabled: Boolean(name),
  })
}

export function useCreateSubstrateActorPool() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; namespace?: string; spec: SubstrateActorPoolSpec }) =>
      api.post<SubstrateActorPool>('/substrate-actor-pools', body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['substrate-actor-pools'] })
    },
  })
}

export function useUpdateSubstrateActorPool() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((state) => state.namespace)
  return useMutation({
    mutationFn: ({ name, spec }: { name: string; spec: SubstrateActorPoolSpec }) =>
      api.put<SubstrateActorPool>(`/substrate-actor-pools/${name}`, { spec }, { namespace }),
    onSuccess: (_data, { name }) => {
      queryClient.invalidateQueries({ queryKey: ['substrate-actor-pools'] })
      queryClient.invalidateQueries({ queryKey: ['substrate-actor-pool', name] })
    },
  })
}

export function useDeleteSubstrateActorPool() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((state) => state.namespace)
  return useMutation({
    mutationFn: (name: string) => api.delete<void>(`/substrate-actor-pools/${name}`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['substrate-actor-pools'] })
    },
  })
}
