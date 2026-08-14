import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { MonitorAction, MonitorCommand, MonitorEvent, MonitorImplementationJob, MonitorItem, MonitorMutation, MonitorRun, MonitorWorkAction, RepositoryMonitor } from '@/schemas/monitor'

interface ListResponse<T> {
  items: T[]
  metadata?: { continue?: string }
}

export interface CreateRepositoryMonitorBody {
  name: string
  namespace?: string
  metadata?: { name?: string; namespace?: string }
  spec: RepositoryMonitor['spec']
}

export function useRepositoryMonitors() {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'repositories', namespace],
    queryFn: () => api.get<ListResponse<RepositoryMonitor>>('/monitors/repositories', { namespace }),
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitor(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'repository', namespace, name],
    queryFn: () => api.get<RepositoryMonitor>(`/monitors/repositories/${name}`, { namespace }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitorRuns(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'runs', namespace, name],
    queryFn: () => api.get<ListResponse<MonitorRun>>(`/monitors/repositories/${name}/runs`, { namespace }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitorItems(name: string, kind = 'pull_request') {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'items', namespace, name, kind],
    queryFn: () => api.get<ListResponse<MonitorItem>>(`/monitors/repositories/${name}/items`, { namespace, kind }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitorActions(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'actions', namespace, name],
    queryFn: () => api.get<ListResponse<MonitorAction>>('/monitors/actions', { namespace, name }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitorCommands(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'commands', namespace, name],
    queryFn: () => api.get<ListResponse<MonitorCommand>>('/monitors/commands', { namespace, name }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitorWorkActions(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'work-actions', namespace, name],
    queryFn: () => api.get<ListResponse<MonitorWorkAction>>('/monitors/work-actions', { namespace, name }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitorImplementationJobs(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'implementation-jobs', namespace, name],
    queryFn: () => api.get<ListResponse<MonitorImplementationJob>>('/monitors/implementation-jobs', { namespace, name }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}

export function useRepositoryMonitorMutations(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'mutations', namespace, name],
    queryFn: () => api.get<ListResponse<MonitorMutation>>('/monitors/mutations', { namespace, name }),
    enabled: !!name,
    refetchInterval: 10000,
  })
}


export interface CreateRepositoryMonitorCommandBody {
  kind: string
  number: number
  intent: string
  targetSHA?: string
}

export function useCreateRepositoryMonitorCommand(name: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (body: CreateRepositoryMonitorCommandBody) => api.post<MonitorCommand>(`/monitors/repositories/${name}/commands`, body, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['monitors', 'commands', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'runs', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'work-actions', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'implementation-jobs', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'mutations', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'items', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'repository', namespace, name] })
    },
  })
}

export function useCreateRepositoryMonitor() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (body: CreateRepositoryMonitorBody) => api.post<RepositoryMonitor>('/monitors/repositories', body),
    onSuccess: (monitor, variables) => {
      const createdNamespace = monitor.metadata.namespace ?? variables.namespace ?? variables.metadata?.namespace ?? namespace
      const createdName = monitor.metadata.name ?? variables.name ?? variables.metadata?.name

      queryClient.invalidateQueries({ queryKey: ['monitors', 'repositories'] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'repositories', createdNamespace] })
      if (createdName) {
        queryClient.invalidateQueries({ queryKey: ['monitors', 'repository', createdNamespace, createdName] })
      }
    },
  })
}

export function useRunRepositoryMonitor(name: string) {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: () => api.post<MonitorRun>(`/monitors/repositories/${name}/runs`, {}, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['monitors', 'runs', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'repository', namespace, name] })
      queryClient.invalidateQueries({ queryKey: ['monitors', 'repositories', namespace] })
    },
  })
}

export interface UpdateRepositoryMonitorBody {
  name: string
  spec: Record<string, unknown>
}

export function useUpdateRepositoryMonitor() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ name, spec }: UpdateRepositoryMonitorBody) =>
      api.put<RepositoryMonitor>(`/monitors/repositories/${name}`, { spec }, { namespace }),
    onSuccess: (_data, { name }) => {
      queryClient.invalidateQueries({ queryKey: ['monitors'] })
      queryClient.invalidateQueries({ queryKey: ['monitor', name] })
    },
  })
}

// Workflow timeline rows for a monitor (GET /monitors/events?name=).
export function useRepositoryMonitorEvents(name: string, refetchInterval: number | false = 10000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitorEvents', name, namespace],
    queryFn: () =>
      api.get<ListResponse<MonitorEvent>>('/monitors/events', { namespace, name, limit: '50' }),
    enabled: Boolean(name),
    refetchInterval,
  })
}

export interface ImplementationJobPatchPreview {
  job: MonitorImplementationJob
  patch: unknown
  contentType?: string
}

// Patch artifact preview for an implementation job. 501 when no artifact
// store is configured; 404 when the job has no patch artifact yet.
export function useImplementationJobPatchPreview(jobID: string, enabled: boolean) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['implementationJobPatch', jobID, namespace],
    queryFn: () =>
      api.get<ImplementationJobPatchPreview>(`/monitors/implementation-jobs/${jobID}/patch-preview`, { namespace }),
    enabled: enabled && Boolean(jobID),
    retry: false,
  })
}
