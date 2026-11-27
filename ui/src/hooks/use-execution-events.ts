import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { executionEventApiPath } from '@/lib/execution-events'
import { useUIStore } from '@/stores/ui'
import type {
  ListExecutionEventsResponse,
  TaskTrace,
  ListTaskApprovalsResponse,
  Approval,
  ForkTaskRequest,
  ForkTaskResponse,
} from '@/schemas/execution-event'

const INITIAL_EVENT_PAGE_LIMIT = '1000'

const TASK_EVENTS_PAGE_KEY = 'taskEventsPage'

export function useTaskEvents(taskId: string, enabled = true, taskUid?: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: [TASK_EVENTS_PAGE_KEY, taskId, namespace, taskUid ?? ''],
    queryFn: () =>
      api.get<ListExecutionEventsResponse>(executionEventApiPath.taskEvents(taskId), {
        namespace,
        after: '0',
        limit: INITIAL_EVENT_PAGE_LIMIT,
      }),
    enabled: enabled && !!taskId,
  })
}

export function useSessionEvents(sessionId: string, enabled = true) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['sessionEvents', sessionId, namespace],
    queryFn: () =>
      api.get<ListExecutionEventsResponse>(executionEventApiPath.sessionEvents(sessionId), {
        namespace,
        after: '0',
        limit: INITIAL_EVENT_PAGE_LIMIT,
      }),
    enabled: enabled && !!sessionId,
  })
}

export function useTaskTrace(
  taskId: string,
  enabled = true,
  taskUid?: string,
  refetchInterval: number | false = false,
) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskTrace', taskId, namespace, taskUid ?? ''],
    queryFn: () =>
      api.get<TaskTrace>(executionEventApiPath.taskTrace(taskId), { namespace }),
    enabled: enabled && !!taskId,
    retry: (failureCount, error) =>
      !(error instanceof ApiError && (error.status === 501 || error.status === 404 || error.status === 403)) &&
      failureCount < 1,
    retryDelay: 100,
    refetchInterval: (query) => {
      const error = query.state.error
      if (error instanceof ApiError && (error.status === 501 || error.status === 404 || error.status === 403)) {
        return false
      }
      return refetchInterval
    },
  })
}

export function useTaskApprovals(
  taskId: string,
  enabled = true,
  pollIntervalMs?: number,
  taskRunning = false,
  taskTerminal = false,
  taskUid?: string,
) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskApprovals', taskId, namespace, taskUid ?? ''],
    queryFn: () =>
      api.get<ListTaskApprovalsResponse>(executionEventApiPath.taskApprovals(taskId), {
        namespace,
      }),
    enabled: enabled && !!taskId,
    retry: false,
    refetchInterval: (query) => {
      if (query.state.error instanceof ApiError && query.state.error.status === 501) return false
      if (!pollIntervalMs) return false
      const approvals = query.state.data?.approvals ?? []
      const hasUnsettledExecution = approvals.some((a) =>
        a.executionOutcome === 'running' ||
        (a.executionOutcome === 'not_started' && !a.executionReason),
      )
      const hasPending = approvals.some((a) => a.status === 'pending')
      return hasUnsettledExecution || (!taskTerminal && (hasPending || taskRunning))
        ? pollIntervalMs
        : false
    },
  })
}

export interface ApprovalDecisionVariables {
  approvalId: string
  decision: 'approve' | 'decline'
  reason?: string
}

export function useDecideApproval(taskId: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ approvalId, decision, reason }: ApprovalDecisionVariables) =>
      api.post<Approval>(
        executionEventApiPath.taskApprovalDecision(taskId, approvalId),
        { decision, reason },
        { namespace },
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['taskApprovals', taskId, namespace] })
      queryClient.invalidateQueries({ queryKey: [TASK_EVENTS_PAGE_KEY, taskId, namespace] })
      queryClient.invalidateQueries({ queryKey: ['taskEvents', taskId, namespace] })
      queryClient.invalidateQueries({ queryKey: ['taskTrace', taskId, namespace] })
    },
  })
}

export interface ForkTaskVariables extends ForkTaskRequest {
  idempotencyKey?: string
}

export function useForkTask(taskId: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ idempotencyKey, ...body }: ForkTaskVariables) =>
      api.post<ForkTaskResponse>(
        executionEventApiPath.taskFork(taskId),
        body,
        { namespace },
        idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : undefined,
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
      queryClient.invalidateQueries({ queryKey: [TASK_EVENTS_PAGE_KEY, taskId, namespace] })
      queryClient.invalidateQueries({ queryKey: ['taskEvents', taskId, namespace] })
      queryClient.invalidateQueries({ queryKey: ['taskTrace', taskId, namespace] })
    },
  })
}
