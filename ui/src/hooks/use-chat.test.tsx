import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useChatStore } from '@/stores/chat'
import { useChatConfig, useSendMessage } from './use-chat'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createSSEResponse(events: Array<{ event: string; data: string }>): Response {
  const text = events.map((e) => `event: ${e.event}\ndata: ${e.data}\n\n`).join('')
  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(text))
      controller.close()
    },
  })
  return new Response(stream, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  })
}

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({
    messages: [],
    currentSessionId: null,
    isStreaming: false,
  })
})

describe('useChatConfig', () => {
  it('returns chat config from API', async () => {
    const { result } = renderHook(() => useChatConfig(), { wrapper: createWrapper() })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toMatchObject({
      enabled: true,
      provider: 'anthropic',
      model: 'claude-sonnet-4-20250514',
    })
  })
})

describe('useSendMessage', () => {
  let fetchSpy: ReturnType<typeof vi.spyOn>
  const originalFetch = globalThis.fetch

  beforeEach(() => {
    fetchSpy = vi.spyOn(globalThis, 'fetch')
  })

  afterEach(() => {
    fetchSpy.mockRestore()
  })

  function mockSSEFetch(events: Array<{ event: string; data: string }>) {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        return Promise.resolve(createSSEResponse(events))
      }
      return originalFetch(input as RequestInfo, init)
    })
  }

  it('adds user message and sets streaming', async () => {
    mockSSEFetch([
      { event: 'message', data: JSON.stringify({ content: 'Hello!' }) },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })

    await act(async () => {
      await result.current('Hi there')
    })

    const messages = useChatStore.getState().messages
    expect(messages[0]).toMatchObject({ role: 'user', content: 'Hi there' })
    // Streaming should be false after completion
    expect(useChatStore.getState().isStreaming).toBe(false)
  })

  it('handles SSE status event — sets sessionId and adds status message', async () => {
    mockSSEFetch([
      {
        event: 'status',
        data: JSON.stringify({ sessionId: 'sess-abc', provider: 'anthropic', model: 'claude' }),
      },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    expect(useChatStore.getState().currentSessionId).toBe('sess-abc')
    const statusMsg = useChatStore.getState().messages.find((m) => m.role === 'status')
    expect(statusMsg).toBeDefined()
    expect(statusMsg!.content).toContain('anthropic')
    expect(statusMsg!.sessionId).toBe('sess-abc')
  })

  it('handles SSE message event — adds assistant message', async () => {
    mockSSEFetch([
      { event: 'message', data: JSON.stringify({ content: 'I am an assistant' }) },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const assistantMsg = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistantMsg).toBeDefined()
    expect(assistantMsg!.content).toBe('I am an assistant')
  })

  it('handles SSE done event — sets usage on last assistant', async () => {
    const usage = { inputTokens: 10, outputTokens: 20, llmCalls: 1 }
    mockSSEFetch([
      { event: 'message', data: JSON.stringify({ content: 'response' }) },
      { event: 'done', data: JSON.stringify({ usage }) },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const assistantMsg = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistantMsg!.usage).toEqual(usage)
  })

  it('handles SSE error event — adds error message', async () => {
    mockSSEFetch([
      { event: 'error', data: JSON.stringify({ error: 'something went wrong' }) },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const errorMsg = useChatStore.getState().messages.find((m) => m.role === 'error')
    expect(errorMsg).toBeDefined()
    expect(errorMsg!.content).toBe('something went wrong')
  })

  it('handles SSE tool_call event — adds tool_call message', async () => {
    mockSSEFetch([
      {
        event: 'tool_call',
        data: JSON.stringify({ id: 'tc-1', name: 'list_tasks', args: { limit: 5 } }),
      },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const tcMsg = useChatStore.getState().messages.find((m) => m.role === 'tool_call')
    expect(tcMsg).toBeDefined()
    expect(tcMsg!.toolName).toBe('list_tasks')
    expect(tcMsg!.toolCallId).toBe('tc-1')
    expect(tcMsg!.toolArgs).toEqual({ limit: 5 })
  })

  it('handles SSE tool_result event — adds tool_result message', async () => {
    mockSSEFetch([
      {
        event: 'tool_result',
        data: JSON.stringify({
          id: 'tc-1',
          name: 'list_tasks',
          result: { success: true, data: [] },
        }),
      },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const trMsg = useChatStore.getState().messages.find((m) => m.role === 'tool_result')
    expect(trMsg).toBeDefined()
    expect(trMsg!.toolName).toBe('list_tasks')
    expect(trMsg!.toolSuccess).toBe(true)
  })

  it('harvests created task names from a successful create_task result onto the assistant turn', async () => {
    mockSSEFetch([
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-1', name: 'create_task', result: { success: true, name: 'task-alpha' } }),
      },
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-2', name: 'create_task', result: { success: true, name: 'task-beta' } }),
      },
      { event: 'message', data: JSON.stringify({ content: 'Created two tasks.' }) },
      { event: 'done', data: JSON.stringify({ usage: { tasksCreated: 2 } }) },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('make two tasks')
    })

    const assistant = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistant?.tasksCreatedNames).toEqual(['task-alpha', 'task-beta'])
  })

  it('does NOT harvest names from non-creation task tools (lookup/update/delete)', async () => {
    mockSSEFetch([
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-1', name: 'get_task', result: { success: true, name: 'existing-task' } }),
      },
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-2', name: 'update_task', result: { success: true, name: 'existing-task' } }),
      },
      { event: 'message', data: JSON.stringify({ content: 'Looked it up.' }) },
      { event: 'done', data: JSON.stringify({ usage: {} }) },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('look up a task')
    })

    const assistant = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistant?.tasksCreatedNames).toBeUndefined()
  })

  it('handles fetch error — adds error message', async () => {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        return Promise.reject(new Error('Network failure'))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const errorMsg = useChatStore.getState().messages.find((m) => m.role === 'error')
    expect(errorMsg).toBeDefined()
    expect(errorMsg!.content).toContain('Network failure')
    expect(useChatStore.getState().isStreaming).toBe(false)
  })

  it('handles non-ok HTTP response — adds error message', async () => {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        return Promise.resolve(new Response('Unauthorized', { status: 401 }))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const errorMsg = useChatStore.getState().messages.find((m) => m.role === 'error')
    expect(errorMsg).toBeDefined()
    expect(errorMsg!.content).toContain('401')
    expect(useChatStore.getState().isStreaming).toBe(false)
  })

  it('handles response with no body — adds error message', async () => {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        const resp = new Response(null, { status: 200 })
        Object.defineProperty(resp, 'body', { value: null })
        return Promise.resolve(resp)
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const errorMsg = useChatStore.getState().messages.find((m) => m.role === 'error')
    expect(errorMsg).toBeDefined()
    expect(errorMsg!.content).toContain('No response body')
    expect(useChatStore.getState().isStreaming).toBe(false)
  })

  it('handles trailing SSE buffer without double newline', async () => {
    // Send SSE data that does NOT end with \n, so parseSSELines trailing check is hit
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        const text = 'event: message\ndata: {"content":"trailing"}'
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode(text))
            controller.close()
          },
        })
        return Promise.resolve(new Response(stream, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        }))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const assistantMsg = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistantMsg).toBeDefined()
    expect(assistantMsg!.content).toBe('trailing')
  })

  it('does not include Authorization header when token is null', async () => {
    useAuthStore.setState({ token: null })

    let capturedAuth: string | null = 'should-be-null'
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        const headers = init?.headers as Record<string, string> | undefined
        capturedAuth = headers?.['Authorization'] ?? null
        return Promise.resolve(createSSEResponse([
          { event: 'message', data: JSON.stringify({ content: 'no-auth' }) },
        ]))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test no token')
    })

    expect(capturedAuth).toBeNull()
  })

  it('skips malformed SSE event data without crashing', async () => {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        // Send malformed JSON in a complete SSE event (with double newline) to hit the catch block
        const text = 'event: message\ndata: {not valid json}\n\nevent: message\ndata: {"content":"after-malformed"}\n\n'
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode(text))
            controller.close()
          },
        })
        return Promise.resolve(new Response(stream, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        }))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test malformed')
    })

    // The malformed event should be skipped, but the valid one should be processed
    const assistantMsg = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistantMsg).toBeDefined()
    expect(assistantMsg!.content).toBe('after-malformed')
  })

  it('skips malformed SSE in trailing buffer', async () => {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        // Trailing buffer with malformed JSON (no double newline at end)
        const text = 'event: message\ndata: {bad json}'
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode(text))
            controller.close()
          },
        })
        return Promise.resolve(new Response(stream, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        }))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test trailing malformed')
    })

    // No assistant message should be added (malformed was skipped)
    const assistantMsg = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistantMsg).toBeUndefined()
    expect(useChatStore.getState().isStreaming).toBe(false)
  })

  it('includes sessionId in request body when currentSessionId is set', async () => {
    useChatStore.setState({ currentSessionId: 'existing-session' })

    let capturedBody: any = null
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        capturedBody = JSON.parse(init?.body as string)
        return Promise.resolve(createSSEResponse([
          { event: 'message', data: JSON.stringify({ content: 'reply' }) },
        ]))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test with session')
    })

    expect(capturedBody).toBeDefined()
    expect(capturedBody.sessionId).toBe('existing-session')
  })
})
