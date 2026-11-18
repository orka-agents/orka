import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

vi.mock('sonner', () => ({ toast: { error: vi.fn(), success: vi.fn() } }))

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useChatStore } from '@/stores/chat'
import { toast } from 'sonner'
import { useChatConfig, useSendMessage } from './use-chat'

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

// A stream the test drives event by event, standing in for a live turn.
function controlledSSE() {
  let controller!: ReadableStreamDefaultController<Uint8Array>
  const encoder = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c
    },
  })
  return {
    response: new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    emit(event: string, data: unknown) {
      try {
        controller.enqueue(encoder.encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`))
      } catch {
        // The client already cancelled the stream.
      }
    },
    close() {
      try {
        controller.close()
      } catch {
        // Already closed or cancelled.
      }
    },
  }
}

function requestURL(input: RequestInfo | URL) {
  return typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
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
    stopRequested: false,
    provider: '',
    model: '',
    activeNamespace: 'default',
    selections: {},
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

  function mockSSEFetch(events: Array<{ event: string; data: string }>, includeDone = true) {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        const completeEvents = includeDone && !events.some(({ event }) => event === 'done' || event === 'error')
          ? [...events, { event: 'done', data: JSON.stringify({ usage: {} }) }]
          : events
        return Promise.resolve(createSSEResponse(completeEvents))
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

  it('reports a stream that ends without done', async () => {
    mockSSEFetch([
      { event: 'message', data: JSON.stringify({ content: 'partial' }) },
    ], false)

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const error = useChatStore.getState().messages.find((message) => message.role === 'error')
    expect(error?.content).toContain('before the terminal done event')
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

  it('harvests created task names from a successful create-task result (data.name) onto the assistant turn', async () => {
    // Production shape: ChatToolResult{ success, data: { name, namespace, phase } }.
    mockSSEFetch([
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-1', name: 'create_agent_task', result: { success: true, data: { name: 'task-alpha', namespace: 'default', phase: 'Pending' } } }),
      },
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-2', name: 'create_ai_task', result: { success: true, data: { name: 'task-beta', namespace: 'default', phase: 'Pending' } } }),
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

  it('also harvests from a flat top-level name (defensive fallback)', async () => {
    mockSSEFetch([
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-1', name: 'create_task', result: { success: true, name: 'flat-task' } }),
      },
      { event: 'message', data: JSON.stringify({ content: 'done' }) },
      { event: 'done', data: JSON.stringify({ usage: { tasksCreated: 1 } }) },
    ])

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('make a task')
    })

    const assistant = useChatStore.getState().messages.find((m) => m.role === 'assistant')
    expect(assistant?.tasksCreatedNames).toEqual(['flat-task'])
  })

  it('does NOT harvest names from non-creation task tools (lookup/update/delete)', async () => {
    mockSSEFetch([
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-1', name: 'get_task', result: { success: true, data: { name: 'existing-task' } } }),
      },
      {
        event: 'tool_result',
        data: JSON.stringify({ id: 'tc-2', name: 'cancel_task', result: { success: true, data: { name: 'existing-task' } } }),
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

  it('drops a turn orphaned by a namespace switch instead of writing into the new transcript', async () => {
    // The response arrives only after the namespace has moved on.
    let release: () => void = () => {}
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    fetchSpy.mockImplementation(async () => {
      await gate
      return createSSEResponse([
        { event: 'status', data: JSON.stringify({ sessionId: 'sess-old', provider: 'openai', model: 'gpt-4o' }) },
        { event: 'message', data: JSON.stringify({ content: 'stale answer' }) },
      ])
    })
    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    let turn: Promise<void> = Promise.resolve()
    act(() => {
      turn = result.current('Hi there')
    })
    expect(useChatStore.getState().isStreaming).toBe(true)
    act(() => {
      useChatStore.getState().selectNamespace('team')
    })
    release()
    await act(async () => {
      await turn
    })
    const state = useChatStore.getState()
    expect(state.currentSessionId).toBeNull()
    expect(state.messages).toEqual([])
    expect(state.isStreaming).toBe(false)
  })

  it('never posts a turn orphaned while the chat config lookup was pending', async () => {
    useChatStore.setState({ provider: '', model: 'gpt-5' })
    let release: () => void = () => {}
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    fetchSpy.mockImplementation(async (input) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
      if (url.includes('/chat/config')) {
        await gate
        return new Response(JSON.stringify({ provider: 'openai', model: 'gpt-4o' }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      throw new Error(`unexpected request ${url}`)
    })
    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    let turn: Promise<void> = Promise.resolve()
    act(() => {
      turn = result.current('Hi there')
    })
    act(() => {
      useChatStore.getState().newSession()
    })
    release()
    await act(async () => {
      await turn
    })
    const posted = fetchSpy.mock.calls.filter(([input]) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
      return url.endsWith('/chat')
    })
    expect(posted).toHaveLength(0)
    expect(useChatStore.getState().messages).toEqual([])
    expect(useChatStore.getState().isStreaming).toBe(false)
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

  it('renders the API error envelope as a readable message, not raw JSON', async () => {
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        return Promise.resolve(new Response(
          JSON.stringify({ error: { code: 500, message: 'no provider "" found and no \'default\' Provider CRD exists' } }),
          { status: 500 },
        ))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('test')
    })

    const errorMsg = useChatStore.getState().messages.find((m) => m.role === 'error')
    expect(errorMsg!.content).toBe('Error 500: no provider "" found and no \'default\' Provider CRD exists')
  })

  it('sends provider and model from the chat store, omitting them when unset', async () => {
    const bodies: any[] = []
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        bodies.push(JSON.parse(init?.body as string))
        return Promise.resolve(createSSEResponse([
          { event: 'message', data: JSON.stringify({ content: 'reply' }) },
        ]))
      }
      return originalFetch(input as RequestInfo, init)
    })

    const { result, rerender } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('no picker')
    })
    expect(bodies[0].provider).toBeUndefined()
    expect(bodies[0].model).toBeUndefined()

    act(() => {
      useChatStore.getState().setProvider('anthropic')
      useChatStore.getState().setModel(' claude-sonnet-4-20250514 ')
    })
    rerender()
    await act(async () => {
      await result.current('with picker')
    })
    expect(bodies[1]).toMatchObject({ provider: 'anthropic', model: 'claude-sonnet-4-20250514' })
  })

  it('pins a model-only override to the server default provider so the server honors it', async () => {
    const bodies: any[] = []
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        bodies.push(JSON.parse(init?.body as string))
        return Promise.resolve(createSSEResponse([
          { event: 'message', data: JSON.stringify({ content: 'reply' }) },
        ]))
      }
      if (url.endsWith('/chat/config')) {
        return Promise.resolve(new Response(JSON.stringify({
          enabled: true, provider: 'anthropic', model: 'claude-sonnet-4-20250514', maxIterations: 1, maxDuration: '1m', maxTasksPerTurn: 1, maxConcurrent: 1, availableTools: [],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      }
      return originalFetch(input as RequestInfo, init)
    })
    useChatStore.setState({ provider: '', model: 'claude-opus-4-1' })

    const wrapper = createWrapper()
    const { result: config } = renderHook(() => useChatConfig(), { wrapper })
    await waitFor(() => expect(config.current.isSuccess).toBe(true))
    const { result } = renderHook(() => useSendMessage(), { wrapper })
    await act(async () => {
      await result.current('model only')
    })
    expect(bodies[0]).toMatchObject({ provider: 'anthropic', model: 'claude-opus-4-1' })
  })

  it('pins a model-only override even when the chat config has not loaded yet', async () => {
    const bodies: any[] = []
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        bodies.push(JSON.parse(init?.body as string))
        return Promise.resolve(createSSEResponse([
          { event: 'message', data: JSON.stringify({ content: 'reply' }) },
        ]))
      }
      if (url.endsWith('/chat/config')) {
        return Promise.resolve(new Response(JSON.stringify({
          enabled: true, provider: 'anthropic', model: 'claude-sonnet-4-20250514', maxIterations: 1, maxDuration: '1m', maxTasksPerTurn: 1, maxConcurrent: 1, availableTools: [],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      }
      return originalFetch(input as RequestInfo, init)
    })
    useChatStore.setState({ provider: '', model: 'claude-opus-4-1' })

    // No useChatConfig subscriber: the config is not cached when the send starts.
    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    await act(async () => {
      await result.current('model only, config cold')
    })
    expect(bodies[0]).toMatchObject({ provider: 'anthropic', model: 'claude-opus-4-1' })
  })

  it('drops a model-only override when the server has no default provider to pin it to', async () => {
    const bodies: any[] = []
    fetchSpy.mockImplementation((input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.endsWith('/chat') && init?.method === 'POST') {
        bodies.push(JSON.parse(init?.body as string))
        return Promise.resolve(createSSEResponse([
          { event: 'message', data: JSON.stringify({ content: 'reply' }) },
        ]))
      }
      if (url.endsWith('/chat/config')) {
        return Promise.resolve(new Response(JSON.stringify({
          enabled: true, provider: '', model: '', maxIterations: 1, maxDuration: '1m', maxTasksPerTurn: 1, maxConcurrent: 1, availableTools: [],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      }
      return originalFetch(input as RequestInfo, init)
    })
    useChatStore.setState({ provider: '', model: 'claude-opus-4-1' })

    const wrapper = createWrapper()
    const { result: config } = renderHook(() => useChatConfig(), { wrapper })
    await waitFor(() => expect(config.current.isSuccess).toBe(true))
    const { result } = renderHook(() => useSendMessage(), { wrapper })
    await act(async () => {
      await result.current('model only')
    })
    expect(bodies[0].provider).toBeUndefined()
    expect(bodies[0].model).toBeUndefined()
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

  describe('ending a turn early', () => {
    // Routes POST /chat to a controlled stream and records DELETE /chat/{id}.
    // `onDelete` plays the server: by default the cancelled turn ends its
    // stream with the interruption error, as HandleCancelChat does.
    function mockTurn(options: { deleteStatus?: number; deleteBody?: unknown } = {}) {
      const sse = controlledSSE()
      const deletes: Array<{ url: string; headers: Record<string, string> }> = []
      const posts: unknown[] = []
      fetchSpy.mockImplementation(async (input, init) => {
        const url = requestURL(input as RequestInfo | URL)
        if (init?.method === 'DELETE') {
          deletes.push({ url, headers: (init.headers ?? {}) as Record<string, string> })
          const status = options.deleteStatus ?? 204
          if (status === 204) {
            sse.emit('error', { error: 'chat turn interrupted: context canceled' })
            sse.close()
            return new Response(null, { status })
          }
          return new Response(JSON.stringify(options.deleteBody ?? {}), { status })
        }
        if (url.endsWith('/chat') && init?.method === 'POST') {
          posts.push(JSON.parse(init.body as string))
          return sse.response
        }
        throw new Error(`unexpected request ${url}`)
      })
      return { sse, deletes, posts }
    }

    it('Stop cancels the server turn with DELETE and starts the next message in a new session', async () => {
      const { sse, deletes } = mockTurn()
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      let turn: Promise<void> = Promise.resolve()
      act(() => {
        turn = result.current('Hi there')
      })
      sse.emit('status', { sessionId: 'sess-1', provider: 'openai', model: 'gpt-4o' })
      await waitFor(() => expect(useChatStore.getState().currentSessionId).toBe('sess-1'))

      act(() => {
        useChatStore.getState().requestStop()
      })
      await act(async () => {
        await turn
      })

      expect(deletes).toHaveLength(1)
      expect(deletes[0].url).toBe('/api/v1/chat/sess-1?namespace=default')
      expect(deletes[0].headers.Authorization).toBe('Bearer test-token')
      const state = useChatStore.getState()
      // The interruption error is replaced by the stop outcome.
      expect(state.messages.filter((m) => m.role === 'error')).toEqual([])
      expect(state.messages[state.messages.length - 1]).toMatchObject({
        role: 'status',
        content: expect.stringContaining('Stopped'),
      })
      expect(state.currentSessionId).toBeNull()
      expect(state.isStreaming).toBe(false)
      expect(state.stopRequested).toBe(false)
    })

    it('Stop before the session ID is known cancels as soon as the status event arrives', async () => {
      const { sse, deletes } = mockTurn()
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      let turn: Promise<void> = Promise.resolve()
      act(() => {
        turn = result.current('Hi there')
      })
      await waitFor(() => expect(fetchSpy).toHaveBeenCalled())
      act(() => {
        useChatStore.getState().requestStop()
      })
      expect(deletes).toHaveLength(0)
      sse.emit('status', { sessionId: 'sess-late', provider: 'openai', model: 'gpt-4o' })
      await act(async () => {
        await turn
      })
      expect(deletes.map((d) => d.url)).toEqual(['/api/v1/chat/sess-late?namespace=default'])
      expect(useChatStore.getState().currentSessionId).toBeNull()
    })

    it('a continuing chat cancels with its known session ID right away', async () => {
      useChatStore.setState({ currentSessionId: 'sess-known' })
      const { deletes, posts } = mockTurn()
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      let turn: Promise<void> = Promise.resolve()
      act(() => {
        turn = result.current('Follow-up')
      })
      await waitFor(() => expect(posts).toHaveLength(1))
      act(() => {
        useChatStore.getState().requestStop()
      })
      await act(async () => {
        await turn
      })
      expect(deletes.map((d) => d.url)).toEqual(['/api/v1/chat/sess-known?namespace=default'])
    })

    it('New Chat during a turn cancels it server-side, not just the fetch', async () => {
      const { sse, deletes } = mockTurn()
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      let turn: Promise<void> = Promise.resolve()
      act(() => {
        turn = result.current('Hi there')
      })
      sse.emit('status', { sessionId: 'sess-new-chat', provider: 'openai', model: 'gpt-4o' })
      await waitFor(() => expect(useChatStore.getState().currentSessionId).toBe('sess-new-chat'))
      act(() => {
        useChatStore.getState().newSession()
      })
      await act(async () => {
        await turn
      })
      expect(deletes.map((d) => d.url)).toEqual(['/api/v1/chat/sess-new-chat?namespace=default'])
      expect(useChatStore.getState().messages).toEqual([])
      expect(useChatStore.getState().isStreaming).toBe(false)
    })

    it('reports a failed cancel of a turn New Chat orphaned and offers a retry', async () => {
      vi.mocked(toast.error).mockClear()
      const { sse, deletes } = mockTurn({ deleteStatus: 403, deleteBody: { error: { code: 403, message: 'not authorized' } } })
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      act(() => {
        void result.current('Hi there')
      })
      sse.emit('status', { sessionId: 'sess-orphan', provider: 'openai', model: 'gpt-4o' })
      await waitFor(() => expect(useChatStore.getState().currentSessionId).toBe('sess-orphan'))
      act(() => {
        useChatStore.getState().newSession()
      })

      // The discarded transcript cannot show it, so the failure is a toast.
      await waitFor(() => expect(toast.error).toHaveBeenCalledWith(
        'Could not stop the previous reply: not authorized',
        expect.objectContaining({ action: expect.objectContaining({ label: 'Retry' }) }),
      ))
      expect(useChatStore.getState().messages).toEqual([])
      const retry = vi.mocked(toast.error).mock.calls[0][1] as { action: { onClick: () => void } }
      act(() => retry.action.onClick())
      await waitFor(() => expect(deletes).toHaveLength(2))
      expect(deletes[1].url).toBe('/api/v1/chat/sess-orphan?namespace=default')
      sse.close()
    })

    it('a turn orphaned before its status event keeps reading until it can cancel, in its own namespace', async () => {
      const { sse, deletes } = mockTurn()
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      let turn: Promise<void> = Promise.resolve()
      act(() => {
        turn = result.current('Hi there')
      })
      await waitFor(() => expect(fetchSpy).toHaveBeenCalled())
      act(() => {
        useChatStore.getState().selectNamespace('team')
      })
      sse.emit('status', { sessionId: 'sess-orphan', provider: 'openai', model: 'gpt-4o' })
      sse.emit('message', { content: 'stale answer' })
      await act(async () => {
        await turn
      })
      expect(deletes.map((d) => d.url)).toEqual(['/api/v1/chat/sess-orphan?namespace=default'])
      // Nothing from the orphaned turn reaches the new namespace's transcript.
      expect(useChatStore.getState().messages).toEqual([])
      expect(useChatStore.getState().currentSessionId).toBeNull()
    })

    it('does not cancel a turn that already finished', async () => {
      const { sse, deletes } = mockTurn()
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      let turn: Promise<void> = Promise.resolve()
      act(() => {
        turn = result.current('Hi there')
      })
      sse.emit('status', { sessionId: 'sess-done', provider: 'openai', model: 'gpt-4o' })
      sse.emit('message', { content: 'answer' })
      sse.emit('done', { usage: {} })
      sse.close()
      await act(async () => {
        await turn
      })
      act(() => {
        useChatStore.getState().newSession()
      })
      expect(deletes).toHaveLength(0)
    })

    it('reports a failed Stop and re-arms it while the turn keeps running', async () => {
      const { sse, deletes } = mockTurn({ deleteStatus: 403, deleteBody: { error: { code: 403, message: 'not authorized' } } })
      const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
      let turn: Promise<void> = Promise.resolve()
      act(() => {
        turn = result.current('Hi there')
      })
      sse.emit('status', { sessionId: 'sess-viewer', provider: 'openai', model: 'gpt-4o' })
      await waitFor(() => expect(useChatStore.getState().currentSessionId).toBe('sess-viewer'))
      act(() => {
        useChatStore.getState().requestStop()
      })
      await waitFor(() =>
        expect(useChatStore.getState().messages.some((m) => m.content === 'Could not stop the turn: not authorized')).toBe(true),
      )
      expect(useChatStore.getState().stopRequested).toBe(false)
      expect(useChatStore.getState().isStreaming).toBe(true)
      sse.emit('message', { content: 'finished anyway' })
      sse.emit('done', { usage: {} })
      sse.close()
      await act(async () => {
        await turn
      })
      expect(deletes).toHaveLength(1)
      expect(useChatStore.getState().currentSessionId).toBe('sess-viewer')
      expect(useChatStore.getState().isStreaming).toBe(false)
    })
  })

  it('ignores a second send while a turn is in flight (double Enter)', async () => {
    const { response, emit, close } = controlledSSE()
    let posts = 0
    fetchSpy.mockImplementation(async (input, init) => {
      if (requestURL(input as RequestInfo | URL).endsWith('/chat') && init?.method === 'POST') {
        posts += 1
        if (posts > 1) {
          return new Response(JSON.stringify({ error: { code: 409, message: 'chat session is busy' } }), { status: 409 })
        }
        return response
      }
      throw new Error('unexpected request')
    })
    const { result } = renderHook(() => useSendMessage(), { wrapper: createWrapper() })
    let first: Promise<void> = Promise.resolve()
    let second: Promise<void> = Promise.resolve()
    act(() => {
      first = result.current('Hi')
      second = result.current('Hi')
    })
    await act(async () => {
      await second
    })
    expect(useChatStore.getState().isStreaming).toBe(true)
    expect(useChatStore.getState().messages.filter((m) => m.role === 'user')).toHaveLength(1)
    emit('done', { usage: {} })
    close()
    await act(async () => {
      await first
    })
    expect(posts).toBe(1)
    expect(useChatStore.getState().messages.some((m) => m.role === 'error')).toBe(false)
    expect(useChatStore.getState().isStreaming).toBe(false)
  })
})
