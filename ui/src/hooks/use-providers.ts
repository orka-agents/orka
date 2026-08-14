import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import type { Provider, ProviderListItem, ProviderSpec } from '@/schemas/provider'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useProviderList(refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['providers', namespace],
    queryFn: () => api.get<ListResponse<ProviderListItem>>('/providers', { namespace }),
    refetchInterval,
  })
}

export function useProvider(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['provider', name, namespace],
    queryFn: () => api.get<Provider>(`/providers/${name}`, { namespace }),
    enabled: Boolean(name),
  })
}

export function useCreateProvider() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; namespace?: string; spec: ProviderSpec }) =>
      api.post<Provider>('/providers', body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['providers'] })
    },
  })
}

export function useUpdateProvider() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ name, spec }: { name: string; spec: ProviderSpec }) =>
      api.put<Provider>(`/providers/${name}`, { spec }, { namespace }),
    onSuccess: (_data, { name }) => {
      queryClient.invalidateQueries({ queryKey: ['providers'] })
      queryClient.invalidateQueries({ queryKey: ['provider', name] })
    },
  })
}

export function useDeleteProvider() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (name: string) => api.delete<void>(`/providers/${name}`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['providers'] })
    },
  })
}
