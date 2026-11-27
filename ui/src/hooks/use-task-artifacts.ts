import { useQuery } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { API_BASE_URL } from '@/lib/constants'
import { listArtifactsResponseSchema } from '@/schemas/artifact'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'

const taskArtifactsPath = (taskId: string) =>
  `/tasks/${encodeURIComponent(taskId)}/artifacts`

export function taskArtifactDownloadUrl(taskId: string, filename: string, namespace: string) {
  const ns = namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''
  return `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/artifacts/${encodeURIComponent(filename)}${ns}`
}

export async function downloadTaskArtifact(taskId: string, filename: string, namespace: string) {
  const token = useAuthStore.getState().token
  const res = await fetch(taskArtifactDownloadUrl(taskId, filename, namespace), {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  if (!res.ok) throw new ApiError(res.status, `failed to download ${filename}`)
  const blob = await res.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

export function useTaskArtifacts(
  taskId: string,
  enabled = true,
  taskUid?: string,
  refetchInterval: number | false = false,
) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskArtifacts', taskId, namespace, taskUid ?? ''],
    queryFn: async () => {
      const raw = await api.get<unknown>(taskArtifactsPath(taskId), { namespace })
      return listArtifactsResponseSchema.parse(raw)
    },
    enabled: enabled && !!taskId,
    retry: (failureCount, error) =>
      !(error instanceof ApiError && (error.status === 501 || error.status === 404)) && failureCount < 3,
    refetchInterval: (query) => {
      const error = query.state.error
      if (error instanceof ApiError && (error.status === 501 || error.status === 404)) return false
      return refetchInterval
    },
  })
}
