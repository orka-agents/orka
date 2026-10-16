import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { pollUnlessForbidden, retryUnlessForbidden, walkAllPages, type ListResponse } from '@/lib/list-api'
import { useUIStore } from '@/stores/ui'
import type { DroppedFinding, PatchProposal, RepositoryScan, ReviewSlice, ScanRun, SecurityFinding, ThreatModel } from '@/schemas/security'

const ALL_FINDINGS_PAGE_LIMIT = '100'

export interface FindingsFilters {
  sliceID?: string
  category?: string
  severity?: string
  validationStatus?: string
  state?: string
  recommended?: string
  limit?: string
  cursor?: string
}

export function useRepositoryScans() {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'repositories', namespace],
    queryFn: () => api.get<ListResponse<RepositoryScan>>('/security/repositories', { namespace }),
    retry: retryUnlessForbidden,
    refetchInterval: pollUnlessForbidden(10000),
  })
}

// A 403 or 404 does not clear on a retry.
const retryUnlessDeniedOrMissing = (failureCount: number, error: unknown) =>
  !isForbiddenError(error) && !isNotFoundError(error) && failureCount < 3

export function useRepositoryScan(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'repository', namespace, name],
    queryFn: () => api.get<RepositoryScan>(`/security/repositories/${name}`, { namespace }),
    enabled: !!name,
    retry: retryUnlessDeniedOrMissing,
    // A forbidden repository stays forbidden, and one that 404s after it
    // loaded was deleted; polling those only spams the API. One never seen
    // keeps polling: the API reads RepositoryScans through the controller
    // cache, so a just-created one can 404 briefly.
    refetchInterval: (query) => {
      if (isForbiddenError(query.state.error)) return false
      if (isNotFoundError(query.state.error) && query.state.data !== undefined) return false
      return 10000
    },
  })
}

export function useCreateRepositoryScan() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: Record<string, unknown>) => api.post<RepositoryScan>('/security/repositories', body),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['security', 'repositories'] }) },
  })
}

export function useThreatModel(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'threat-model', namespace, name],
    queryFn: () => api.get<ThreatModel>(`/security/repositories/${name}/threat-model`, { namespace }),
    enabled: !!name,
  })
}

// expectedVersion is the version the edit started from, or 0 when no model
// existed; the server answers 409 when the model has moved on since (or, for 0,
// when one has been created meanwhile).
interface ThreatModelEdit {
  content: string
  source?: string
  expectedVersion: number
}

export function useUpdateThreatModel(name: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  const mutation = useMutation({
    // The target travels in the variables, fixed when the save is made. A
    // pending mutation runs with the latest render's options, so a namespace
    // or repository switch before the PUT goes out (a save waiting for the
    // network, say) must not send it, or file the saved model, anywhere else.
    mutationFn: ({ target, edit }: { target: { namespace: string; name: string }; edit: ThreatModelEdit }) =>
      api.put<ThreatModel>(`/security/repositories/${target.name}/threat-model`, edit, { namespace: target.namespace }),
    onSuccess: (saved, { target }) => {
      // The saved version is the base for the next edit, even before the refetch lands.
      if (saved) queryClient.setQueryData(['security', 'threat-model', target.namespace, target.name], saved)
      queryClient.invalidateQueries({ queryKey: ['security', 'threat-model', target.namespace, target.name] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repository', target.namespace, target.name] })
    },
  })
  return { ...mutation, mutateAsync: (edit: ThreatModelEdit) => mutation.mutateAsync({ target: { namespace, name }, edit }) }
}

export function useScanRuns(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'scans', namespace, name],
    queryFn: () => api.get<ListResponse<ScanRun>>(`/security/repositories/${name}/scans`, { namespace }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useReviewSlices(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'slices', namespace, name],
    queryFn: () => api.get<ListResponse<ReviewSlice>>(`/security/repositories/${name}/slices`, { namespace }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useDroppedFindings(name: string, scanRunID?: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'dropped-findings', namespace, name, scanRunID],
    queryFn: () => api.get<ListResponse<DroppedFinding>>(`/security/repositories/${name}/dropped-findings`, {
      namespace,
      ...(scanRunID ? { scanRunID } : {}),
    }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRunSecurityScan(name: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<ScanRun>(`/security/repositories/${name}/scans`, undefined, { namespace }),
    onMutate: () => ({ namespace, name }),
    onSuccess: (_data, _variables, target) => {
      queryClient.invalidateQueries({ queryKey: ['security', 'scans', target.namespace, target.name] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repository', target.namespace, target.name] })
    },
  })
}

export function useFindings(name: string, filters: FindingsFilters = {}) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'findings', namespace, name, filters],
    queryFn: () => api.get<ListResponse<SecurityFinding>>(`/security/repositories/${name}/findings`, { namespace, ...filters }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useAllFindings(name: string, filters: Omit<FindingsFilters, 'limit' | 'cursor'> = {}) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'findings', namespace, 'all', name, filters],
    // The findings API names its continuation parameter `cursor`.
    queryFn: () => walkAllPages(
      (cursor) => api.get<ListResponse<SecurityFinding>>(`/security/repositories/${name}/findings`, {
        namespace,
        ...filters,
        limit: ALL_FINDINGS_PAGE_LIMIT,
        ...(cursor ? { cursor } : {}),
      }),
      { subject: 'finding list' },
    ),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useFinding(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'finding', namespace, id],
    queryFn: () => api.get<SecurityFinding>(`/security/findings/${id}`, { namespace }),
    enabled: !!id,
    retry: retryUnlessDeniedOrMissing,
    // Findings come from the security store, so a 404 is final.
    refetchInterval: (query) =>
      isForbiddenError(query.state.error) || isNotFoundError(query.state.error) ? false : 10000,
  })
}

export function useDismissFinding(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<void>(`/security/findings/${id}/dismiss`, undefined, { namespace }),
    onMutate: () => ({ namespace, id }),
    onSuccess: (_data, _variables, target) => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', target.namespace, target.id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings', target.namespace] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repositories', target.namespace] })
    },
  })
}

export function useReopenFinding(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<void>(`/security/findings/${id}/reopen`, undefined, { namespace }),
    onMutate: () => ({ namespace, id }),
    onSuccess: (_data, _variables, target) => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', target.namespace, target.id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings', target.namespace] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repositories', target.namespace] })
    },
  })
}

export function useGeneratePatch(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<PatchProposal>(`/security/findings/${id}/patch`, undefined, { namespace }),
    onMutate: () => ({ namespace, id }),
    onSuccess: (_data, _variables, target) => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', target.namespace, target.id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'patches', target.namespace, target.id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings', target.namespace] })
    },
  })
}

export function useValidateFinding(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<void>(`/security/findings/${id}/validate`, undefined, { namespace }),
    onMutate: () => ({ namespace, id }),
    onSuccess: (_data, _variables, target) => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', target.namespace, target.id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings', target.namespace] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repositories', target.namespace] })
    },
  })
}

export function usePatchProposals(id: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'patches', namespace, id],
    queryFn: () => api.get<ListResponse<PatchProposal>>(`/security/findings/${id}/patches`, { namespace }),
    enabled: !!id,
    refetchInterval: 10000,
  })
}

export function useCreatePullRequest(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<{ prURL: string; prNumber: number; status: string }>(`/security/findings/${id}/pull-request`, undefined, { namespace }),
    onMutate: () => ({ namespace, id }),
    onSuccess: (_data, _variables, target) => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', target.namespace, target.id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'patches', target.namespace, target.id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings', target.namespace] })
    },
  })
}
