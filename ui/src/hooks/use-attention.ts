import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { executionEventApiPath } from '@/lib/execution-events'
import { pollUnlessForbidden, retryUnlessForbidden } from '@/lib/list-api'
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

/** Decidable approvals across the namespace's Tasks, soonest deadline first. */
export function usePendingApprovals(limit = 100, refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['approvals', 'pending', namespace, limit],
    enabled: Boolean(token),
    queryFn: () => api.get<AttentionResponse>('/approvals', { namespace, status: 'pending', limit: String(limit) }),
    retry: (failureCount, error) =>
      !(error instanceof ApiError && (error.status === 403 || error.status === 501)) && failureCount < 3,
    refetchInterval: (query) => {
      const error = query.state.error
      if (error instanceof ApiError && (error.status === 403 || error.status === 501)) return false
      return refetchInterval
    },
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
