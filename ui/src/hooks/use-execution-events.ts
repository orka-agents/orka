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

// Request the server's maximum page size for the initial load. The list endpoint
// defaults to a small page (100); without this, completed tasks with more events
// would permanently show only the first page, and live tasks would rely entirely
// on the stream to backfill. MaxExecutionEventLimit (1000) covers realistic tasks
// in one page; the live stream still fills anything beyond it.
const INITIAL_EVENT_PAGE_LIMIT = '1000'

// Cache key prefix for this hook's single-page replay query. Deliberately
// DISTINCT from the ['taskEvents', ...] key used by the full-history paged hook
// in use-tasks.ts: the two fetchers return different shapes (one bounded page vs.
// the whole paged history), so sharing a key would let a refetch of one overwrite
// the other's cache — e.g. this one-page query could clobber the paged hook's
// complete list with a partial page, and the paged hook would then resume from a
// truncated tail. They only diverged into the same key once both started keying
// by uid, so they get separate namespaces here.
const TASK_EVENTS_PAGE_KEY = 'taskEventsPage'

// List the initial (replay) page of a task's execution events. Live updates are
// layered on top by the streaming hook; this query provides the static history
// and a refetchable fallback when streaming is unavailable.
//
// taskUid scopes the cache to a specific Task object. A Task deleted and
// recreated with the same name+namespace gets a new metadata.uid; threading it
// into the query key means the replacement task starts from a clean cache entry
// instead of inheriting the prior task's latestSeq/events. Mirrors the uid-keyed
// task events hook in use-tasks.ts.
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
      !(error instanceof ApiError && error.status === 501) && failureCount < 1,
    retryDelay: 100,
    refetchInterval: (query) =>
      query.state.error instanceof ApiError && query.state.error.status === 501
        ? false
        : refetchInterval,
  })
}

// Poll while an approval is still pending OR the task can still emit new
// approvals (it is not in a terminal phase). Polling stops once the task has
// reached a terminal phase, OR nothing is pending and the task isn't running —
// so a settled panel doesn't refetch forever while a still-running task's first
// ApprovalRequested is never missed. A terminal task is the key stop condition:
// its pending approvals render read-only (the backend rejects decisions), and no
// further events will flip their status, so polling them would never terminate.
// Pass pollIntervalMs to enable polling; omit it to never poll.
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
      // A settled task won't change a pending approval (it's read-only), so stop
      // polling instead of refetching the same pending row indefinitely.
      if (taskTerminal) return false
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
      // Refresh BOTH event caches: this hook's single-page replay query and the
      // full-history paged query in use-tasks.ts (broad ['taskEvents', ...] prefix,
      // which the Overview/Execution panels read). A decision appends events to the
      // task timeline that both should reflect.
      queryClient.invalidateQueries({ queryKey: [TASK_EVENTS_PAGE_KEY, taskId, namespace] })
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
      // Refresh BOTH event caches (single-page replay + the full-history paged
      // query in use-tasks.ts that the Overview/Execution panels read).
      queryClient.invalidateQueries({ queryKey: [TASK_EVENTS_PAGE_KEY, taskId, namespace] })
      queryClient.invalidateQueries({ queryKey: ['taskEvents', taskId, namespace] })
      // A fork appends TaskForkRequested/TaskForkCreated to this (source) task's
      // timeline, and the Trace tab derives its Fork provenance section from that
      // timeline. Invalidate the trace too so an already-loaded Trace tab reflects
      // the new fork instead of going stale until a manual refresh.
      queryClient.invalidateQueries({ queryKey: ['taskTrace', taskId, namespace] })
    },
  })
}
