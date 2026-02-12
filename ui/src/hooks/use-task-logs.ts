import { useState, useEffect, useRef, useCallback } from 'react'
import { useAuthStore } from '@/stores/auth'
import { API_BASE_URL } from '@/lib/constants'
import { useUIStore } from '@/stores/ui'
import type { TaskPhase } from '@/schemas/task'

function isRunningPhase(phase?: TaskPhase): boolean {
  return phase === 'Running' || phase === 'Pending'
}

export function useTaskLogs(taskId: string, enabled = true, taskPhase?: TaskPhase) {
  const [logs, setLogs] = useState<string[]>([])
  const [isStreaming, setIsStreaming] = useState(false)
  const [isLive, setIsLive] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const fetchLogs = useCallback(async () => {
    if (!enabled || !taskId) return

    const token = useAuthStore.getState().token
    const namespace = useUIStore.getState().namespace
    const running = isRunningPhase(taskPhase)

    try {
      abortRef.current?.abort()
      const controller = new AbortController()
      abortRef.current = controller
      setIsStreaming(true)
      setError(null)

      const params = new URLSearchParams({ namespace })
      if (running) {
        params.set('tailLines', '200')
      }

      const response = await fetch(
        `${API_BASE_URL}/tasks/${taskId}/logs?${params.toString()}`,
        {
          headers: token ? { Authorization: `Bearer ${token}` } : {},
          signal: controller.signal,
        }
      )

      if (!response.ok) {
        throw new Error(`Failed to fetch logs: ${response.statusText}`)
      }

      if (running) {
        // For running tasks, parse JSON response and replace logs
        const data = await response.json()
        const text: string = data.logs ?? ''
        setLogs(text.split('\n').filter(Boolean))
        setIsLive(true)
      } else {
        // For completed tasks, use streaming reader or text fallback
        const reader = response.body?.getReader()
        if (!reader) {
          const text = await response.text()
          try {
            const data = JSON.parse(text)
            setLogs((data.logs ?? '').split('\n').filter(Boolean))
          } catch {
            setLogs(text.split('\n').filter(Boolean))
          }
        } else {
          const decoder = new TextDecoder()
          let buffer = ''
          const allLines: string[] = []

          while (true) {
            const { done, value } = await reader.read()
            if (done) break

            buffer += decoder.decode(value, { stream: true })
            const lines = buffer.split('\n')
            buffer = lines.pop() || ''
            allLines.push(...lines.filter(Boolean))
          }

          if (buffer) allLines.push(buffer)

          // Try parsing as JSON in case of completed task response
          if (allLines.length === 1) {
            try {
              const data = JSON.parse(allLines[0])
              if (data.logs) {
                setLogs(data.logs.split('\n').filter(Boolean))
              } else {
                setLogs(allLines)
              }
            } catch {
              setLogs(allLines)
            }
          } else {
            setLogs(allLines)
          }
        }
        setIsLive(false)
      }
    } catch (err) {
      if (err instanceof Error && err.name !== 'AbortError') {
        setError(err.message)
      }
    } finally {
      setIsStreaming(false)
    }
  }, [taskId, enabled, taskPhase])

  useEffect(() => {
    fetchLogs()

    // Set up polling for running tasks
    if (pollRef.current) {
      clearInterval(pollRef.current)
      pollRef.current = null
    }

    if (enabled && isRunningPhase(taskPhase)) {
      pollRef.current = setInterval(fetchLogs, 3000)
    }

    return () => {
      abortRef.current?.abort()
      if (pollRef.current) {
        clearInterval(pollRef.current)
        pollRef.current = null
      }
    }
  }, [fetchLogs, enabled, taskPhase])

  // When task completes, stop polling and do a final fetch
  useEffect(() => {
    if (!isRunningPhase(taskPhase) && isLive) {
      setIsLive(false)
      if (pollRef.current) {
        clearInterval(pollRef.current)
        pollRef.current = null
      }
      fetchLogs()
    }
  }, [taskPhase, isLive, fetchLogs])

  const clear = useCallback(() => setLogs([]), [])

  return { logs, isStreaming, isLive, error, refetch: fetchLogs, clear }
}
