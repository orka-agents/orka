import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api-client'
import { API_BASE_URL } from '@/lib/constants'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { Skill, SkillListItem, SkillSpec } from '@/schemas/skill'

interface ListResponse<T> {
  items: T[]
  metadata: { continue?: string; remainingItemCount?: number }
}

export function useSkillList(refetchInterval: number | false = 15000) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['skills', namespace],
    queryFn: () => api.get<ListResponse<SkillListItem>>('/skills', { namespace }),
    refetchInterval,
  })
}

export function useSkill(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['skill', name, namespace],
    queryFn: () => api.get<Skill>(`/skills/${name}`, { namespace }),
    enabled: Boolean(name),
  })
}

// GET /skills/:name/content returns text/markdown, not JSON, so it bypasses
// the JSON api client.
export function useSkillContent(name: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['skillContent', name, namespace],
    queryFn: async () => {
      const token = useAuthStore.getState().token
      const params = new URLSearchParams({ namespace })
      const response = await fetch(`${API_BASE_URL}/skills/${name}/content?${params}`, {
        headers: token ? { Authorization: `Bearer ${token}` } : undefined,
      })
      if (!response.ok) {
        throw new Error(`failed to load skill content (${response.status})`)
      }
      return response.text()
    },
    enabled: Boolean(name),
  })
}

export function useCreateSkill() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; namespace?: string; spec: SkillSpec }) =>
      api.post<Skill>('/skills', body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['skills'] })
    },
  })
}

export function useUpdateSkill() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: ({ name, spec }: { name: string; spec: SkillSpec }) =>
      api.put<Skill>(`/skills/${name}`, { spec }, { namespace }),
    onSuccess: (_data, { name }) => {
      queryClient.invalidateQueries({ queryKey: ['skills'] })
      queryClient.invalidateQueries({ queryKey: ['skill', name] })
      queryClient.invalidateQueries({ queryKey: ['skillContent', name] })
    },
  })
}

export function useDeleteSkill() {
  const queryClient = useQueryClient()
  const namespace = useUIStore((s) => s.namespace)
  return useMutation({
    mutationFn: (name: string) => api.delete<void>(`/skills/${name}`, { namespace }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['skills'] })
    },
  })
}
