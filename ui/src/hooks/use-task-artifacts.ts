import { useQuery } from '@tanstack/react-query'
import { ApiError, api } from '@/lib/api-client'
import { API_BASE_URL } from '@/lib/constants'
import { listArtifactsResponseSchema } from '@/schemas/artifact'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'

// Relative path for the shared api client (prepends API_BASE_URL itself).
export const taskArtifactsPath = (taskId: string) =>
  `/tasks/${encodeURIComponent(taskId)}/artifacts`

// Absolute, namespace-safe download URL for a single artifact. Filename and
// namespace are URL-encoded so unusual names can't break routing.
export function taskArtifactDownloadUrl(taskId: string, filename: string, namespace: string) {
  const ns = namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''
  return `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/artifacts/${encodeURIComponent(filename)}${ns}`
}

// Download an artifact through the authenticated fetch path. A bare <a download>
// cannot carry the bearer token (auth is header-only, no cookie), so the API
// returns 401; this fetches the blob with the Authorization header and saves it
// via a transient object URL. Throws on non-OK so callers can surface an error.
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

// List artifact metadata for a task. Backend returns { artifacts: [] }, and 501
// when the artifact store is disabled — treat that as a stable "unsupported"
// empty rather than retrying forever.
export function useTaskArtifacts(taskId: string, enabled = true, taskUid?: string) {
  const namespace = useUIStore((s) => s.namespace)
  return useQuery({
    queryKey: ['taskArtifacts', taskId, namespace, taskUid ?? ''],
    queryFn: async () => {
      const raw = await api.get<unknown>(taskArtifactsPath(taskId), { namespace })
      return listArtifactsResponseSchema.parse(raw)
    },
    enabled: enabled && !!taskId,
    retry: (failureCount, error) =>
      !(error instanceof ApiError && error.status === 501) && failureCount < 3,
  })
}
