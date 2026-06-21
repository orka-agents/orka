import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
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

// Request the server's maximum page size for the initial load. The list endpoint
// defaults to a small page (100); without this, completed tasks with more events
// would permanently show only the first page, and live tasks would rely entirely
// on the stream to backfill. MaxExecutionEventLimit (1000) covers realistic tasks
// in one page; the live stream still fills anything beyond it.
const INITIAL_EVENT_PAGE_LIMIT = '1000'

// List the initial (replay) page of a task's execution events. Live updates are
// layered on top by the streaming hook; this query provides the static history
// and a refetchable fallback when streaming is unavailable.
export function useTaskEvents(taskId: string, enabled = true) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskEvents', taskId, namespace],
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

export function useTaskTrace(taskId: string, enabled = true) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskTrace', taskId, namespace],
    queryFn: () =>
      api.get<TaskTrace>(executionEventApiPath.taskTrace(taskId), { namespace }),
    enabled: enabled && !!taskId,
  })
}

// Poll while an approval is still pending OR the task can still emit new
// approvals (it is not in a terminal phase). Polling stops only once nothing is
// pending AND the task has finished, so a settled panel doesn't refetch forever
// while a still-running task's first ApprovalRequested is never missed. Pass
// pollIntervalMs to enable polling; omit it to never poll.
export function useTaskApprovals(
  taskId: string,
  enabled = true,
  pollIntervalMs?: number,
  taskRunning = false,
) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskApprovals', taskId, namespace],
    queryFn: () =>
      api.get<ListTaskApprovalsResponse>(executionEventApiPath.taskApprovals(taskId), {
        namespace,
      }),
    enabled: enabled && !!taskId,
    refetchInterval: (query) => {
      if (!pollIntervalMs) return false
      const approvals = query.state.data?.approvals ?? []
      const hasPending = approvals.some((a) => a.status === 'pending')
      return hasPending || taskRunning ? pollIntervalMs : false
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
      queryClient.invalidateQueries({ queryKey: ['taskEvents', taskId, namespace] })
      queryClient.invalidateQueries({ queryKey: ['taskTrace', taskId, namespace] })
    },
  })
}

// Variables for a fork submission. idempotencyKey, when provided, is sent as the
// Idempotency-Key header so the backend collapses retries of the same logical
// fork (blank-name path) onto one task instead of minting a duplicate.
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
      queryClient.invalidateQueries({ queryKey: ['taskEvents', taskId, namespace] })
    },
  })
}
