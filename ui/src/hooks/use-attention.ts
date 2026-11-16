import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { executionEventApiPath } from '@/lib/execution-events'
import { pageParams, pollUnlessForbidden, retryUnlessForbidden } from '@/lib/list-api'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { AttentionResponse } from '@/schemas/attention'
import type { Approval } from '@/schemas/execution-event'

/** The namespace's "Needs you" ranking: one page, highest priority first. */
export function useAttention(limit = 50, refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['attention', namespace, limit],
    enabled: Boolean(token),
    queryFn: () => api.get<AttentionResponse>('/attention', { namespace, limit: String(limit) }),
    retry: retryUnlessForbidden,
    refetchInterval: pollUnlessForbidden(refetchInterval),
  })
}

/** Pages of the board's attention walk; later items stay unresolved. */
export const maxAttentionWalkPages = 10

export interface AttentionWalk extends AttentionResponse {
  /** More attention items exist past the walk bound; their Tasks are unclassified. */
  truncated: boolean
}

/**
 * Every attention item the board needs, walked page by page (bounded), so a
 * Task ranked past the first page is never shown as Done. The ranking totals
 * and incomplete flag come from the first page; scan bounds hit on any page
 * are kept.
 */
export function useAttentionAll(pageLimit = 200, refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['attention', 'all', namespace, pageLimit],
    enabled: Boolean(token),
    queryFn: async (): Promise<AttentionWalk> => {
      let first: AttentionResponse | undefined
      let sources: AttentionResponse['sources'] | undefined
      const items: AttentionResponse['items'] = []
      const reasons = new Set<string>()
      const seen = new Set<string>()
      let incomplete = false
      let next: string | undefined
      let pages = 0
      do {
        const page = await api.get<AttentionResponse>('/attention', pageParams({ namespace, limit: String(pageLimit) }, next))
        first ??= page
        // A source any page skipped stays skipped for the whole walk.
        sources = {
          approvals: sources && sources.approvals !== 'included' ? sources.approvals : page.sources?.approvals,
          monitors: sources && sources.monitors !== 'included' ? sources.monitors : page.sources?.monitors,
        }
        items.push(...page.items)
        incomplete ||= page.incomplete
        for (const reason of page.incompleteReasons ?? []) reasons.add(reason)
        next = page.metadata?.continue || undefined
        if (next && seen.has(next)) throw new Error('attention pagination repeated continuation cursor')
        if (next) seen.add(next)
        pages += 1
      } while (next && pages < maxAttentionWalkPages)
      return { ...first!, sources: sources!, items, incomplete, incompleteReasons: [...reasons].sort(), metadata: {}, truncated: Boolean(next) }
    },
    retry: retryUnlessForbidden,
    refetchInterval: pollUnlessForbidden(refetchInterval),
  })
}

/**
 * A 403 or 501 from the inbox will not clear on its own: it stops retries
 * and polling, and it replaces any cached rows so stale decision controls
 * are never left on screen.
 */
export const isTerminalInboxError = (error: unknown) => error instanceof ApiError && (error.status === 403 || error.status === 501)

/**
 * Decidable approvals across the namespace's Tasks, soonest deadline first,
 * one page at a time: later pages follow metadata.continue on demand.
 */
export function usePendingApprovals(limit = 100, refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useInfiniteQuery({
    queryKey: ['approvals', 'pending', namespace, limit],
    enabled: Boolean(token),
    queryFn: ({ pageParam }) =>
      api.get<AttentionResponse>('/approvals', pageParams({ namespace, status: 'pending', limit: String(limit) }, pageParam || undefined)),
    initialPageParam: '',
    getNextPageParam: (lastPage) => lastPage.metadata?.continue || undefined,
    retry: (failureCount, error) => !isTerminalInboxError(error) && failureCount < 3,
    refetchInterval: (query) => (isTerminalInboxError(query.state.error) ? false : refetchInterval),
  })
}

export interface InboxDecisionVariables {
  taskName: string
  taskUID: string
  approvalId: string
  decision: 'approve' | 'decline'
  reason?: string
}

export type InboxDecisionResult =
  | { outcome: 'decided'; approval: Approval }
  | { outcome: 'alreadyDecided'; message: string }

/**
 * Posts a decision from the inbox through the existing per-Task decision
 * route, fenced to the Task UID the inbox listed so a later Task that reuses
 * the name is never decided. A 409 or 410 means someone (or the Task's end)
 * settled it first: the first decision wins.
 */
export function useInboxDecision() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: async ({ taskName, taskUID, approvalId, decision, reason }: InboxDecisionVariables): Promise<InboxDecisionResult> => {
      try {
        const approval = await api.post<Approval>(
          executionEventApiPath.taskApprovalDecision(taskName, approvalId),
          { decision, reason },
          { namespace, uid: taskUID },
        )
        return { outcome: 'decided', approval }
      } catch (error) {
        if (error instanceof ApiError && (error.status === 409 || error.status === 410)) {
          return { outcome: 'alreadyDecided', message: error.message }
        }
        throw error
      }
    },
    onSettled: (_data, _error, variables) => {
      queryClient.invalidateQueries({ queryKey: ['approvals'] })
      queryClient.invalidateQueries({ queryKey: ['attention'] })
      queryClient.invalidateQueries({ queryKey: ['taskApprovals', variables.taskName, namespace] })
    },
  })
}
