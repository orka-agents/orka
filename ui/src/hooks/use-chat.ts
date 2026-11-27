import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback } from 'react'
import { toast } from 'sonner'
import { api, apiErrorMessage } from '@/lib/api-client'
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
  if (currentEvent && currentData) {
    events.push({ event: currentEvent, data: currentData })
  }
  return events
}

async function cancelChatTurn(sessionId: string, namespace: string, token: string | null) {
  const url = `${API_BASE_URL}/chat/${encodeURIComponent(sessionId)}?${new URLSearchParams({ namespace })}`
  const response = await fetch(url, {
    method: 'DELETE',
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  if (!response.ok && response.status !== 404) {
    throw new Error(apiErrorMessage(await response.text().catch(() => `HTTP ${response.status}`)))
  }
}

export function useSendMessage() {
  const token = useAuthStore((s) => s.token)
  const namespace = useUIStore((s) => s.namespace)
  const queryClient = useQueryClient()
  const {
    currentSessionId,
    provider,
    model,
    addMessage,
    setSessionId,
    setStreaming,
    setUsageOnLastAssistant,
  } = useChatStore()

  return useCallback(
    async (messageText: string) => {
      if (useChatStore.getState().isStreaming) return

      const createdTaskNames: string[] = []

      const epoch = useChatStore.getState().turnEpoch
      const live = () => useChatStore.getState().turnEpoch === epoch
      const controller = new AbortController()

      let turnSessionId = currentSessionId
      let requestSent = false
      let cancelWanted = false
      let cancelSent = false
      let orphaned = false
      let stopped = false
      let terminalEventSeen = false
      let cancelPromise: Promise<void> | undefined
      const reportOrphanedCancelFailure = (sessionId: string, err: unknown) => {
        toast.error(`Could not stop the previous reply: ${err instanceof Error ? err.message : 'Unknown error'}`, {
          description: 'It may still run tools or create tasks.',
          action: {
            label: 'Retry',
            onClick: () => {
              cancelChatTurn(sessionId, namespace, token).catch((retryErr: unknown) => reportOrphanedCancelFailure(sessionId, retryErr))
            },
          },
        })
      }
      const cancelServerTurn = () => {
        cancelWanted = true
        if (!requestSent || cancelSent || !turnSessionId || terminalEventSeen) return
        cancelSent = true
        const sessionId = turnSessionId
        cancelPromise = cancelChatTurn(sessionId, namespace, token).then(
          () => {
            if (!stopped || !live()) return
            if (useChatStore.getState().currentSessionId === sessionId) setSessionId(null)
            addMessage({
              id: generateMessageId(),
              role: 'status',
              content: 'Stopped. This chat and its history were deleted; your next message starts a new chat.',
              timestamp: new Date().toISOString(),
            })
          },
          (err: unknown) => {
            if (orphaned) {
              reportOrphanedCancelFailure(sessionId, err)
              return
            }
            if (!stopped || !live()) return
            stopped = false
            cancelSent = false
            useChatStore.setState({ stopRequested: false })
            addMessage({
              id: generateMessageId(),
              role: 'error',
              content: `Could not stop the turn: ${err instanceof Error ? err.message : 'Unknown error'}`,
              timestamp: new Date().toISOString(),
            })
          },
        )
      }
      const unsubscribeTurnControl = useChatStore.subscribe((state) => {
        if (state.turnEpoch !== epoch) {
          if (orphaned) return
          orphaned = true
          cancelServerTurn()
          if (turnSessionId || !requestSent) controller.abort()
        } else if (state.stopRequested && !stopped) {
          stopped = true
          cancelServerTurn()
        }
      })

      function handleSSEEvent(event: string, data: string) {
        if (event === 'status' && !turnSessionId) {
          const status = JSON.parse(data) as SSEStatusEvent
          turnSessionId = status.sessionId || null
          if (cancelWanted) cancelServerTurn()
        }
        if (!live()) return
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
            const lowerName = tr.name.toLowerCase()
            const isCreateTaskTool = lowerName.includes('create') && lowerName.includes('task')
            if (isCreateTaskTool && result && result.success === true) {
              const payload = (result.data ?? result) as Record<string, unknown>
              const name =
                (typeof payload.name === 'string' && payload.name) ||
                (typeof payload.taskName === 'string' && payload.taskName) ||
                (typeof (payload.task as Record<string, unknown>)?.name === 'string' &&
                  ((payload.task as Record<string, unknown>).name as string)) ||
                undefined
              if (name && !createdTaskNames.includes(name)) createdTaskNames.push(name)
            }
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
            terminalEventSeen = true
            setUsageOnLastAssistant(done.usage, createdTaskNames)
            break
          }
          case 'error': {
            const err = JSON.parse(data) as { error: string }
            terminalEventSeen = true
            if (stopped) break
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
        let effectiveProvider = provider
        if (!effectiveProvider && model.trim()) {
          const config = await queryClient.ensureQueryData({
            queryKey: ['chatConfig'],
            queryFn: () => api.get<ChatConfig>('/chat/config'),
            staleTime: 60 * 1000,
          })
          effectiveProvider = config.provider ?? ''
        }
        if (effectiveProvider) {
          body.provider = effectiveProvider
          if (model.trim()) body.model = model.trim()
        }
        if (!live()) return
        if (stopped) {
          addMessage({
            id: generateMessageId(),
            role: 'status',
            content: 'Stopped before the message was sent.',
            timestamp: new Date().toISOString(),
          })
          return
        }

        requestSent = true
        const responsePromise = fetch(`${API_BASE_URL}/chat`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
          },
          body: JSON.stringify(body),
          signal: controller.signal,
        })
        if (cancelWanted) cancelServerTurn()
        const response = await responsePromise

        if (!live() && (turnSessionId || !response.ok || !response.body)) {
          await response.body?.cancel().catch(() => {})
          return
        }
        if (!response.ok) {
          const errText = await response.text().catch(() => 'Unknown error')
          if (!live() || stopped) return
          addMessage({
            id: generateMessageId(),
            role: 'error',
            content: `Error ${response.status}: ${apiErrorMessage(errText)}`,
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
          if (!live() && (turnSessionId || done)) {
            await reader.cancel().catch(() => {})
            return
          }
          if (done) break

          buffer += decoder.decode(value, { stream: true })

          const lastDoubleNewline = buffer.lastIndexOf('\n\n')
          if (lastDoubleNewline === -1) continue

          const complete = buffer.slice(0, lastDoubleNewline + 2)
          buffer = buffer.slice(lastDoubleNewline + 2)

          const events = parseSSELines(complete)
          for (const { event, data } of events) {
            try {
              handleSSEEvent(event, data)
            } catch {
            }
          }
          if (!live() && turnSessionId) {
            await reader.cancel().catch(() => {})
            return
          }
        }

        if (buffer.trim()) {
          const events = parseSSELines(buffer)
          for (const { event, data } of events) {
            try {
              handleSSEEvent(event, data)
            } catch {
            }
          }
        }

        if (!live()) return
        if (!terminalEventSeen && !stopped) {
          addMessage({
            id: generateMessageId(),
            role: 'error',
            content: 'Stream ended before the terminal done event',
            timestamp: new Date().toISOString(),
          })
        }
      } catch (err) {
        if (!live()) return
        addMessage({
          id: generateMessageId(),
          role: 'error',
          content: `Connection error: ${err instanceof Error ? err.message : 'Unknown'}`,
          timestamp: new Date().toISOString(),
        })
      } finally {
        unsubscribeTurnControl()
        if (cancelPromise && live()) await cancelPromise.catch(() => {})
        if (live()) setStreaming(false)
      }
    },
    [token, namespace, currentSessionId, provider, model, queryClient, addMessage, setSessionId, setStreaming, setUsageOnLastAssistant],
  )
}
