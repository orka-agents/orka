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
  // Handle trailing event without blank line
  if (currentEvent && currentData) {
    events.push({ event: currentEvent, data: currentData })
  }
  return events
}

// DELETE /chat/{sessionId} is the only way to stop a server-side turn: the
// server keeps running a turn (tools, Task creation) when its stream is
// dropped, so aborting the fetch alone does not stop it. The endpoint waits for
// the active turn to stop and then ends the session. It carries the token
// that started the turn, so a turn orphaned by an identity change is cancelled
// as the identity that owns it (and a 401 cannot clear the new token).
async function cancelChatTurn(sessionId: string, namespace: string, token: string | null) {
  const url = `${API_BASE_URL}/chat/${encodeURIComponent(sessionId)}?${new URLSearchParams({ namespace })}`
  const response = await fetch(url, {
    method: 'DELETE',
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  // 404: the session is already gone, which is what cancelling wants.
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
      // One turn at a time. The store is read here rather than through a
      // render-time snapshot: two sends in the same tick (a double Enter)
      // both see the stale snapshot, and the second would post a duplicate
      // turn whose 409 then ended the first turn's streaming state.
      if (useChatStore.getState().isStreaming) return

      // Task names created during this turn, harvested from create_task tool
      // results, so the assistant message can render cross-linking chips.
      const createdTaskNames: string[] = []

      // The turn owns the transcript only while the store's epoch is the one
      // it started under; a namespace switch or New Chat mid-stream orphans
      // it, after which nothing from this response may reach the store.
      const epoch = useChatStore.getState().turnEpoch
      const live = () => useChatStore.getState().turnEpoch === epoch
      const controller = new AbortController()

      // Ending a turn early (Stop, New Chat, or a namespace or identity
      // switch) cancels it server-side. A continuing chat knows its session
      // up front; a new chat learns it from the first `status` event, so a
      // cancel wanted before then is sent as soon as that event arrives
      // (an orphaned turn keeps reading its stream until it does).
      let turnSessionId = currentSessionId
      let requestSent = false
      let cancelWanted = false
      let cancelSent = false
      let orphaned = false
      // Stop keeps the transcript: the turn reports the outcome itself.
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
        // After done/error the server turn is over; cancelling would only
        // delete the finished session.
        if (!requestSent || cancelSent || !turnSessionId || terminalEventSeen) return
        cancelSent = true
        const sessionId = turnSessionId
        cancelPromise = cancelChatTurn(sessionId, namespace, token).then(
          () => {
            if (!stopped || !live()) return
            // The cancelled session is gone server-side; the next message
            // must start a new one rather than reuse its reserved name.
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
              // New Chat (or a namespace or identity switch) discarded this
              // turn's transcript, but its server turn keeps running, so say
              // so outside the transcript and offer a retry.
              reportOrphanedCancelFailure(sessionId, err)
              return
            }
            if (!stopped || !live()) return
            // The turn keeps running; re-arm Stop so it can be retried.
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
          // Without a session ID the stream must still be read to learn it.
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
            // Harvest names ONLY from successful task-CREATION tools, so the
            // turn links to tasks it actually created. Lookups/updates/deletes
            // that merely reference a task by name must not appear as "created"
            // chips. Require both "create" and "task" in the tool name (any
            // order: create_task, task_create, create_agent_task, ...).
            const lowerName = tr.name.toLowerCase()
            const isCreateTaskTool = lowerName.includes('create') && lowerName.includes('task')
            if (isCreateTaskTool && result && result.success === true) {
              // The backend wraps payloads as ChatToolResult{success, data:{...}}
              // (internal/tools/registry.go), and create-task tools put the task
              // name in `data.name`. Read there first; fall back to top-level
              // fields defensively for any tool that returns a flatter shape.
              // (Named `payload` to avoid shadowing the outer SSE `data` string.)
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
            // A stopped turn ends with the server's interruption error; the
            // cancel request reports the outcome instead.
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
        // The server resolves `model` only alongside an explicit provider; a
        // model-only override would be silently ignored. Pin it to the
        // server's configured default provider, or drop it when there is
        // none to pin to. The config is resolved here (cached by the query
        // client) so a send racing the initial /chat/config load does not
        // silently drop a restored model-only override.
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
        // The config lookup above may have yielded; a New Chat, namespace
        // switch, or Stop in the meantime means this turn must never reach
        // the server (it could still run tools or create Tasks before being
        // cancelled).
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
        // A continuing chat's turn is cancellable as soon as it is posted.
        if (cancelWanted) cancelServerTurn()
        const response = await responsePromise

        if (!live() && (turnSessionId || !response.ok || !response.body)) {
          await response.body?.cancel().catch(() => {})
          return
        }
        if (!response.ok) {
          const errText = await response.text().catch(() => 'Unknown error')
          // Reading the error body yielded; a reset in the meantime means
          // this error belongs to the orphaned turn, not the new transcript.
          // A turn stopped before the server took it reports through the
          // cancel instead (its session was ended under the request).
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
          // The epoch is rechecked before acting on the result: a read that
          // resolves after a reset — including the final done — must not
          // flush this turn's remaining buffer into the new transcript. An
          // orphaned new chat keeps reading only until it learns its session
          // ID (handleSSEEvent sends the cancel then).
          if (!live() && (turnSessionId || done)) {
            await reader.cancel().catch(() => {})
            return
          }
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
          // An orphan that just learned its session ID has sent its cancel.
          if (!live() && turnSessionId) {
            await reader.cancel().catch(() => {})
            return
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
        // A stopped turn stays "streaming" until its cancel settles, so a new
        // message cannot race the session deletion with the old session ID.
        if (cancelPromise && live()) await cancelPromise.catch(() => {})
        // An orphaned turn's transcript was already reset; its streaming flag
        // was cleared with it and must not be touched again.
        if (live()) setStreaming(false)
      }
    },
    [token, namespace, currentSessionId, provider, model, queryClient, addMessage, setSessionId, setStreaming, setUsageOnLastAssistant],
  )
}
