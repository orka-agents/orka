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

export function useTaskApprovals(taskId: string, enabled = true, refetchInterval?: number) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskApprovals', taskId, namespace],
    queryFn: () =>
      api.get<ListTaskApprovalsResponse>(executionEventApiPath.taskApprovals(taskId), {
        namespace,
      }),
    enabled: enabled && !!taskId,
    refetchInterval,
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

export function useForkTask(taskId: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (body: ForkTaskRequest) =>
      api.post<ForkTaskResponse>(executionEventApiPath.taskFork(taskId), body, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
      queryClient.invalidateQueries({ queryKey: ['taskEvents', taskId, namespace] })
    },
  })
}
