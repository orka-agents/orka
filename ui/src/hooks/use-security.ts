import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { PatchProposal, RepositoryScan, ScanRun, SecurityFinding, ThreatModel } from '@/schemas/security'

interface ListResponse<T> {
  items: T[]
  metadata?: { continue?: string }
}

const ALL_FINDINGS_PAGE_LIMIT = '100'

export interface FindingsFilters {
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
    refetchInterval: 10000,
  })
}

export function useRepositoryScan(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['security', 'repository', namespace, name],
    queryFn: () => api.get<RepositoryScan>(`/security/repositories/${name}`, { namespace }),
    enabled: !!name,
    refetchInterval: 10000,
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

export function useUpdateThreatModel(name: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (body: { content: string; source?: string }) =>
      api.put<ThreatModel>(`/security/repositories/${name}/threat-model`, body, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['security', 'threat-model', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repository', namespace, name] })
    },
  })
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

export function useRunSecurityScan(name: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<ScanRun>(`/security/repositories/${name}/scans`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['security', 'scans', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repository', namespace, name] })
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
    queryKey: ['security', 'findings', 'all', namespace, name, filters],
    queryFn: async () => {
      const items: SecurityFinding[] = []
      let cursor: string | undefined

      do {
        const page = await api.get<ListResponse<SecurityFinding>>(`/security/repositories/${name}/findings`, {
          namespace,
          ...filters,
          limit: ALL_FINDINGS_PAGE_LIMIT,
          ...(cursor ? { cursor } : {}),
        })
        items.push(...page.items)
        cursor = page.metadata?.continue
      } while (cursor)

      return { items, metadata: {} } satisfies ListResponse<SecurityFinding>
    },
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
    refetchInterval: 10000,
  })
}

export function useDismissFinding(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<void>(`/security/findings/${id}/dismiss`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', namespace, id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings'] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repositories'] })
    },
  })
}

export function useReopenFinding(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<void>(`/security/findings/${id}/reopen`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', namespace, id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings'] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repositories'] })
    },
  })
}

export function useGeneratePatch(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<PatchProposal>(`/security/findings/${id}/patch`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', namespace, id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'patches', namespace, id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings'] })
    },
  })
}

export function useValidateFinding(id: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<void>(`/security/findings/${id}/validate`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', namespace, id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings'] })
      queryClient.invalidateQueries({ queryKey: ['security', 'repositories'] })
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
    mutationFn: () => api.post<{ prURL: string; prNumber: number; status: string }>(`/security/findings/${id}/pull-request`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['security', 'finding', namespace, id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'patches', namespace, id] })
      queryClient.invalidateQueries({ queryKey: ['security', 'findings'] })
    },
  })
}
