import { useQuery } from '@tanstack/react-query'
import { useAuthStore } from '@/stores/auth'

export interface ReadyzResponse {
  status: string
  checks?: Record<string, string>
}

export interface CompatModel {
  id: string
  object?: string
  created?: number
  owned_by?: string
}

// /healthz and /readyz live at the server root (outside /api/v1) and require
// no auth. Readiness intentionally returns 503 with a JSON body when a
// dependency is down, so parse the body on every status.
export function useReadyz(refetchInterval: number | false = 15000) {
  return useQuery({
    queryKey: ['readyz'],
    queryFn: async (): Promise<ReadyzResponse> => {
      const response = await fetch('/readyz')
      const body = (await response.json().catch(() => ({ status: 'unknown' }))) as ReadyzResponse
      return body
    },
    refetchInterval,
  })
}

export function useHealthz() {
  return useQuery({
    queryKey: ['healthz'],
    queryFn: async () => {
      const response = await fetch('/healthz')
      if (!response.ok) throw new Error(`healthz returned ${response.status}`)
      return (await response.json()) as { status: string }
    },
  })
}

// Model catalog through the OpenAI-compatible surface. IDs come back both as
// provider/model and bare model names.
export function useCompatModels() {
  return useQuery({
    queryKey: ['compat-models'],
    queryFn: async (): Promise<CompatModel[]> => {
      const token = useAuthStore.getState().token
      const response = await fetch('/openai/v1/models', {
        headers: token ? { Authorization: `Bearer ${token}` } : undefined,
      })
      if (!response.ok) throw new Error(`model catalog returned ${response.status}`)
      const body = (await response.json()) as { data?: CompatModel[] }
      return body.data ?? []
    },
    staleTime: 5 * 60 * 1000,
  })
}
