import { useState, useEffect, useRef, useCallback } from 'react'
import { useAuthStore } from '@/stores/auth'
import { API_BASE_URL } from '@/lib/constants'
import { useUIStore } from '@/stores/ui'

export function useTaskLogs(taskId: string, enabled = true) {
  const [logs, setLogs] = useState<string[]>([])
  const [isStreaming, setIsStreaming] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const abortRef = useRef<AbortController | null>(null)

  const fetchLogs = useCallback(async () => {
    if (!enabled || !taskId) return

    const token = useAuthStore.getState().token
    const namespace = useUIStore.getState().namespace

    try {
      abortRef.current?.abort()
      const controller = new AbortController()
      abortRef.current = controller
      setIsStreaming(true)
      setError(null)

      const response = await fetch(
        `${API_BASE_URL}/tasks/${taskId}/logs?namespace=${namespace}`,
        {
          headers: token ? { Authorization: `Bearer ${token}` } : {},
          signal: controller.signal,
        }
      )

      if (!response.ok) {
        throw new Error(`Failed to fetch logs: ${response.statusText}`)
      }

      const reader = response.body?.getReader()
      if (!reader) {
        const text = await response.text()
        setLogs(text.split('\n').filter(Boolean))
        setIsStreaming(false)
        return
      }

      const decoder = new TextDecoder()
      let buffer = ''

      while (true) {
        const { done, value } = await reader.read()
        if (done) break

        buffer += decoder.decode(value, { stream: true })
        const lines = buffer.split('\n')
        buffer = lines.pop() || ''

        if (lines.length > 0) {
          setLogs(prev => [...prev, ...lines.filter(Boolean)])
        }
      }

      if (buffer) {
        setLogs(prev => [...prev, buffer])
      }
    } catch (err) {
      if (err instanceof Error && err.name !== 'AbortError') {
        setError(err.message)
      }
    } finally {
      setIsStreaming(false)
    }
  }, [taskId, enabled])

  useEffect(() => {
    fetchLogs()
    return () => { abortRef.current?.abort() }
  }, [fetchLogs])

  const clear = useCallback(() => setLogs([]), [])

  return { logs, isStreaming, error, refetch: fetchLogs, clear }
}
