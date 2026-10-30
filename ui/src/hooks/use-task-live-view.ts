import { useEffect, useState } from 'react'
import { API_BASE_URL } from '@/lib/constants'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'

export type TaskLiveView =
  | { status: 'off' }
  | { status: 'waiting' }
  | { status: 'live'; src: string; frameTime?: string }

/** How often the latest frame is fetched; the worker sends at most two a second. */
export const LIVE_VIEW_POLL_MS = 1000

// uid names the Task the page shows: the controller answers 404 for another
// Task since created under the same name.
export function taskLiveViewUrl(taskId: string, namespace: string, uid = '') {
  const query = new URLSearchParams()
  if (namespace) query.set('namespace', namespace)
  if (uid) query.set('uid', uid)
  const search = query.toString()
  return `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/live${search ? `?${search}` : ''}`
}

// frameDataUrl turns a frame into a data: URL. The dashboard's Content
// Security Policy allows data: images but not blob: URLs.
function frameDataUrl(frame: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error)
    reader.readAsDataURL(new Blob([frame], { type: 'image/jpeg' }))
  })
}

// Polls the latest screencast frame of a running browser Task. A frame is a
// JPEG fetched with the bearer token (auth is header-only, so a bare <img src>
// would get 401) and shown as a data: URL. 204 means live view is on
// but no page has repainted yet; 404 means this Task has no live view, which
// ends the polling. An unchanged frame answers 304 and keeps the current one.
// The view belongs to one Task: switching Tasks or namespaces never shows the
// previous Task's frame, even while the new one waits for its first. Paused,
// it keeps the current frame and stops polling, like the page's Following.
export function useTaskLiveView(taskId: string, enabled: boolean, paused = false, uid = ''): TaskLiveView {
  const namespace = useUIStore((s) => s.namespace)
  const identity = `${namespace}/${taskId}/${uid}`
  const [state, setState] = useState<{ identity: string; view: TaskLiveView }>({
    identity,
    view: { status: 'waiting' },
  })

  useEffect(() => {
    if (!enabled || paused || !taskId) return
    let stopped = false
    let etag = ''
    let timer: ReturnType<typeof setTimeout> | undefined
    const abort = new AbortController()

    const poll = async () => {
      try {
        const token = useAuthStore.getState().token
        const headers: Record<string, string> = {}
        if (token) headers.Authorization = `Bearer ${token}`
        if (etag) headers['If-None-Match'] = etag
        const res = await fetch(taskLiveViewUrl(taskId, namespace, uid), { headers, signal: abort.signal, cache: 'no-store' })
        if (stopped) return
        if (res.status === 404 || res.status === 501) {
          setState({ identity, view: { status: 'off' } })
          return
        }
        if (res.status === 200) {
          const src = await frameDataUrl(await res.blob())
          if (stopped) return
          etag = res.headers.get('ETag') ?? ''
          setState({ identity, view: { status: 'live', src, frameTime: res.headers.get('X-Orka-Frame-Time') ?? undefined } })
        } else if (res.status === 204) {
          // No frame now: none has arrived yet, or the last one expired because
          // the worker stopped sending. Either way, an old frame is not current.
          etag = ''
          setState({ identity, view: { status: 'waiting' } })
        }
      } catch {
        // A dropped request is retried on the next tick.
      }
      if (!stopped) timer = setTimeout(poll, LIVE_VIEW_POLL_MS)
    }
    void poll()

    return () => {
      stopped = true
      abort.abort()
      clearTimeout(timer)
    }
  }, [taskId, namespace, uid, identity, enabled, paused])

  if (!enabled) return { status: 'off' }
  return state.identity === identity ? state.view : { status: 'waiting' }
}
