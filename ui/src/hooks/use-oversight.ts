import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, isNotFoundError, isNotImplementedError } from '@/lib/api-client'
import { pageParams } from '@/lib/list-api'
import { useChatConfig } from '@/hooks/use-chat'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type {
  DecisionFeedback,
  DecisionListResponse,
  DecisionView,
  FeedbackResponse,
  GroupsResponse,
  OversightConfig,
  OversightFeedbackLabel,
  PostureResponse,
  ProducersResponse,
  SimilarResponse,
} from '@/schemas/oversight'

const BASE = '/oversight'

/**
 * A 403 or 501 will not clear on its own, and a 400 is the query grammar
 * refusing the search: none of them is retried or polled.
 */
export const isTerminalOversightError = (error: unknown) =>
  error instanceof ApiError && (error.status === 400 || error.status === 403 || error.status === 501)

const retryOversight = (failureCount: number, error: unknown) => !isTerminalOversightError(error) && failureCount < 3

const pollOversight = (interval: number | false) =>
  (query: { state: { error: unknown } }) => (isTerminalOversightError(query.state.error) ? false : interval)

/**
 * Whether this controller serves oversight: true, false, or undefined while
 * unknown. The chat config advertises it; without chat (or on a server that
 * predates the flag) the posture API is asked, and a 501 means off. A 403
 * still means oversight is on: the page then says the token cannot read it.
 */
export function useOversightAvailability(): boolean | undefined {
  const namespace = useUIStore((s) => s.namespace)
  const { data: chatConfig, isError: chatConfigUnavailable } = useChatConfig()
  const flag = chatConfig?.oversightEnabled
  const probeNeeded = flag === undefined && (chatConfigUnavailable || chatConfig !== undefined)
  const probe = useQuery({
    queryKey: ['oversightAvailability', namespace],
    queryFn: () => api.get<unknown>(`${BASE}/posture`, { namespace, window: '1h' }),
    enabled: probeNeeded,
    retry: false,
    staleTime: 60 * 1000,
  })
  if (flag !== undefined) return flag
  if (!probeNeeded) return undefined
  if (probe.isSuccess) return true
  if (probe.isError) return !(isNotImplementedError(probe.error) || isNotFoundError(probe.error))
  return undefined
}

export function useOversightConfig() {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['oversight', 'config', namespace],
    enabled: Boolean(token),
    queryFn: () => api.get<OversightConfig>(`${BASE}/config`, { namespace }),
    retry: retryOversight,
    staleTime: 5 * 60 * 1000,
  })
}

export function useOversightPosture(window = '24h', refetchInterval: number | false = 30000) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['oversight', 'posture', namespace, window],
    enabled: Boolean(token),
    queryFn: () => api.get<PostureResponse>(`${BASE}/posture`, { namespace, window }),
    retry: retryOversight,
    refetchInterval: pollOversight(refetchInterval),
  })
}

export interface DecisionListParams {
  q?: string
  /** Absent or "all" searches all time. */
  window?: string
  limit?: number
  enabled?: boolean
}

/** Decisions matching a query, newest first, one page at a time. */
export function useOversightDecisions({ q = '', window, limit = 50, enabled = true }: DecisionListParams) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useInfiniteQuery({
    queryKey: ['oversight', 'decisions', namespace, q, window ?? 'all', limit],
    enabled: Boolean(token) && enabled,
    queryFn: ({ pageParam }) =>
      api.get<DecisionListResponse>(
        `${BASE}/decisions`,
        pageParams({ namespace, q, window: window && window !== 'all' ? window : '', limit: String(limit) }, pageParam || undefined),
      ),
    initialPageParam: '',
    getNextPageParam: (lastPage) => lastPage.continue || undefined,
    retry: retryOversight,
  })
}

export function useOversightDecision(id: string | undefined) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['oversight', 'decision', namespace, id],
    enabled: Boolean(token) && Boolean(id),
    queryFn: () => api.get<DecisionView>(`${BASE}/decisions/${encodeURIComponent(id!)}`, { namespace }),
    retry: (count, error) => !isNotFoundError(error) && retryOversight(count, error),
  })
}

export function useOversightSimilar(id: string | undefined, enabled: boolean, limit = 25) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['oversight', 'similar', namespace, id, limit],
    enabled: Boolean(token) && Boolean(id) && enabled,
    queryFn: () => api.get<SimilarResponse>(`${BASE}/decisions/${encodeURIComponent(id!)}/similar`, { namespace, limit: String(limit) }),
    retry: retryOversight,
  })
}

export function useOversightGroups(q: string, by: 'agent' | 'task', window: string | undefined, enabled: boolean) {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['oversight', 'groups', namespace, q, by, window ?? 'all'],
    enabled: Boolean(token) && enabled && q.trim() !== '',
    queryFn: () =>
      api.get<GroupsResponse>(`${BASE}/decisions/groups`, { namespace, q, by, window: window && window !== 'all' ? window : '' }),
    retry: retryOversight,
  })
}

/**
 * Operator-declared producers. A 404 or 501 (an older controller without the
 * endpoint) reads as "no declarations", not as an error.
 */
export function useOversightProducers() {
  const namespace = useUIStore((s) => s.namespace)
  const token = useAuthStore((s) => s.token)
  return useQuery({
    queryKey: ['oversight', 'producers', namespace],
    enabled: Boolean(token),
    queryFn: async (): Promise<ProducersResponse> => {
      try {
        return await api.get<ProducersResponse>(`${BASE}/producers`, { namespace })
      } catch (error) {
        if (isNotFoundError(error) || isNotImplementedError(error)) return { namespace, declared: false, producers: [] }
        throw error
      }
    },
    retry: retryOversight,
    staleTime: 60 * 1000,
  })
}

const overridesKey = (namespace: string) => ['oversightFeedbackOverrides', namespace]

/**
 * Labels recorded in this session, by decision ID. Rows that arrived over the
 * live stream are not refetched, so they read their latest label from here.
 */
export function useFeedbackOverrides(): Record<string, DecisionFeedback> {
  const namespace = useUIStore((s) => s.namespace)
  const { data } = useQuery({
    queryKey: overridesKey(namespace),
    queryFn: () => ({}) as Record<string, DecisionFeedback>,
    staleTime: Infinity,
    gcTime: Infinity,
  })
  return data ?? {}
}

function useRecordOverrides() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return (ids: string[], feedback: DecisionFeedback) => {
    queryClient.setQueryData<Record<string, DecisionFeedback>>(overridesKey(namespace), (prev) => {
      const next = { ...(prev ?? {}) }
      for (const id of ids) next[id] = feedback
      return next
    })
    queryClient.invalidateQueries({ queryKey: ['oversight'] })
  }
}

export interface FeedbackVariables {
  label: OversightFeedbackLabel
  note?: string
}

/** Records one person's outcome label. Labels calibrate monitors only. */
export function useOversightFeedback(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  const record = useRecordOverrides()
  return useMutation({
    mutationFn: ({ label, note }: FeedbackVariables) =>
      api.post<FeedbackResponse>(`${BASE}/decisions/${encodeURIComponent(id)}/feedback`, { label, note: note || undefined }, { namespace }),
    onSuccess: (response) => record([id], response.feedback),
  })
}

export function useOversightBulkFeedback() {
  const namespace = useUIStore((s) => s.namespace)
  const record = useRecordOverrides()
  return useMutation({
    mutationFn: ({ ids, label, note }: FeedbackVariables & { ids: string[] }) =>
      api.post<FeedbackResponse>(`${BASE}/decisions/bulk-feedback`, { ids, label, note: note || undefined }, { namespace }),
    onSuccess: (response, variables) => record(variables.ids, response.feedback),
  })
}

/** A decision with any label recorded in this session applied. */
export function withOverride(decision: DecisionView, overrides: Record<string, DecisionFeedback>): DecisionView {
  const feedback = overrides[decision.id]
  return feedback ? { ...decision, feedback } : decision
}
