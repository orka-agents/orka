import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { MonitorCommand, MonitorItem, MonitorRun, RepositoryMonitor } from '@/schemas/monitor'

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

export function useRepositoryMonitorCommands(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['monitors', 'commands', namespace, name],
    queryFn: () => api.get<ListResponse<MonitorCommand>>('/monitors/commands', { namespace, name }),
    enabled: !!name,
    refetchInterval: 10000,
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
