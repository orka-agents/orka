import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { agentRuntimeListSchema, runtimePoolListSchema } from '@/schemas/runtime'
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
