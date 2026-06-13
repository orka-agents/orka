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
