import { useQuery } from '@tanstack/react-query'
import { useCallback } from 'react'
import { api } from '@/lib/api-client'
import { API_BASE_URL } from '@/lib/constants'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { useChatStore, generateMessageId } from '@/stores/chat'
import type {
  ChatConfig,
  ChatRequest,
  SSEStatusEvent,
  SSEToolCallEvent,
  SSEToolResultEvent,
  SSEMessageEvent,
  SSEDoneEvent,
} from '@/schemas/chat'

export function useChatConfig() {
  return useQuery({
    queryKey: ['chatConfig'],
    queryFn: () => api.get<ChatConfig>('/chat/config'),
    staleTime: 60 * 1000,
  })
}

function parseSSELines(text: string): Array<{ event: string; data: string }> {
  const events: Array<{ event: string; data: string }> = []
  let currentEvent = ''
  let currentData = ''

  for (const line of text.split('\n')) {
    if (line.startsWith('event: ')) {
      currentEvent = line.slice(7)
    } else if (line.startsWith('data: ')) {
      currentData = line.slice(6)
    } else if (line === '' && currentEvent) {
      events.push({ event: currentEvent, data: currentData })
      currentEvent = ''
      currentData = ''
    }
  }
  // Handle trailing event without blank line
  if (currentEvent && currentData) {
    events.push({ event: currentEvent, data: currentData })
  }
  return events
}

export function useSendMessage() {
  const token = useAuthStore((s) => s.token)
  const namespace = useUIStore((s) => s.namespace)
  const {
    currentSessionId,
    addMessage,
    setSessionId,
    setStreaming,
    setUsageOnLastAssistant,
  } = useChatStore()

  return useCallback(
    async (messageText: string) => {
      function handleSSEEvent(event: string, data: string) {
        const now = new Date().toISOString()

        switch (event) {
          case 'status': {
            const status = JSON.parse(data) as SSEStatusEvent
            setSessionId(status.sessionId)
            addMessage({
              id: generateMessageId(),
              role: 'status',
              content: `Connected to ${status.provider}/${status.model}`,
              timestamp: now,
              provider: status.provider,
              model: status.model,
              sessionId: status.sessionId,
            })
            break
          }
          case 'tool_call': {
            const tc = JSON.parse(data) as SSEToolCallEvent
            addMessage({
              id: generateMessageId(),
              role: 'tool_call',
              content: tc.name,
              timestamp: now,
              toolCallId: tc.id,
              toolName: tc.name,
              toolArgs: tc.args,
            })
            break
          }
          case 'tool_result': {
            const tr = JSON.parse(data) as SSEToolResultEvent
            const result = tr.result as Record<string, unknown> | undefined
            addMessage({
              id: generateMessageId(),
              role: 'tool_result',
              content: tr.name,
              timestamp: now,
              toolCallId: tr.id,
              toolName: tr.name,
              toolResult: tr.result,
              toolSuccess: result?.success === true,
            })
            break
          }
          case 'message': {
            const msg = JSON.parse(data) as SSEMessageEvent
            addMessage({
              id: generateMessageId(),
              role: 'assistant',
              content: msg.content,
              timestamp: now,
            })
            break
          }
          case 'done': {
            const done = JSON.parse(data) as SSEDoneEvent
            setUsageOnLastAssistant(done.usage)
            break
          }
          case 'error': {
            const err = JSON.parse(data) as { error: string }
            addMessage({
              id: generateMessageId(),
              role: 'error',
              content: err.error,
              timestamp: now,
            })
            break
          }
        }
      }

      // Add user message to store
      addMessage({
        id: generateMessageId(),
        role: 'user',
        content: messageText,
        timestamp: new Date().toISOString(),
      })

      setStreaming(true)

      const body: ChatRequest = {
        message: messageText,
        namespace,
      }
      if (currentSessionId) {
        body.sessionId = currentSessionId
      }

      try {
        const response = await fetch(`${API_BASE_URL}/chat`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
          },
          body: JSON.stringify(body),
        })

        if (!response.ok) {
          const errText = await response.text().catch(() => 'Unknown error')
          addMessage({
            id: generateMessageId(),
            role: 'error',
            content: `Error ${response.status}: ${errText}`,
            timestamp: new Date().toISOString(),
          })
          setStreaming(false)
          return
        }

        if (!response.body) {
          addMessage({
            id: generateMessageId(),
            role: 'error',
            content: 'No response body (streaming not supported)',
            timestamp: new Date().toISOString(),
          })
          setStreaming(false)
          return
        }

        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''

        while (true) {
          const { done, value } = await reader.read()
          if (done) break

          buffer += decoder.decode(value, { stream: true })

          // Parse complete SSE events from buffer
          const lastDoubleNewline = buffer.lastIndexOf('\n\n')
          if (lastDoubleNewline === -1) continue

          const complete = buffer.slice(0, lastDoubleNewline + 2)
          buffer = buffer.slice(lastDoubleNewline + 2)

          const events = parseSSELines(complete)
          for (const { event, data } of events) {
            try {
              handleSSEEvent(event, data)
            } catch {
              // Skip malformed events
            }
          }
        }

        // Process remaining buffer
        if (buffer.trim()) {
          const events = parseSSELines(buffer)
          for (const { event, data } of events) {
            try {
              handleSSEEvent(event, data)
            } catch {
              // Skip malformed events
            }
          }
        }
      } catch (err) {
        addMessage({
          id: generateMessageId(),
          role: 'error',
          content: `Connection error: ${err instanceof Error ? err.message : 'Unknown'}`,
          timestamp: new Date().toISOString(),
        })
      } finally {
        setStreaming(false)
      }
    },
    [token, namespace, currentSessionId, addMessage, setSessionId, setStreaming, setUsageOnLastAssistant],
  )
}
