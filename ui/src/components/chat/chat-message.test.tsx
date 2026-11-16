import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
  }
})

import { http, HttpResponse } from 'msw'
import { act } from 'react'
import { server } from '@/test/mocks/server'
import { render, screen, waitFor } from '@/test/test-utils'
import { ChatMessage } from './chat-message'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useChatStore } from '@/stores/chat'
import type { ChatMessage as ChatMessageType } from '@/schemas/chat'

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false })
})

const userMsg: ChatMessageType = {
  id: 'msg-1',
  role: 'user',
  content: 'Hello',
  timestamp: new Date().toISOString(),
}

const assistantMsg: ChatMessageType = {
  id: 'msg-2',
  role: 'assistant',
  content: 'Hi there!',
  timestamp: new Date().toISOString(),
  usage: { llmCalls: 1, toolCalls: 2, duration: '1.5s' },
}

const statusMsg: ChatMessageType = {
  id: 'msg-3',
  role: 'status',
  content: 'Connected to anthropic/claude-sonnet-4-20250514',
  timestamp: new Date().toISOString(),
}

const errorMsg: ChatMessageType = {
  id: 'msg-4',
  role: 'error',
  content: 'Connection failed',
  timestamp: new Date().toISOString(),
}

const toolCallMsg: ChatMessageType = {
  id: 'msg-5',
  role: 'tool_call',
  content: 'create_task',
  timestamp: new Date().toISOString(),
  toolCallId: 'tc-1',
  toolName: 'create_task',
  toolArgs: { name: 'test' },
}

const toolResultMsg: ChatMessageType = {
  id: 'msg-6',
  role: 'tool_result',
  content: 'create_task',
  timestamp: new Date().toISOString(),
  toolCallId: 'tc-1',
  toolName: 'create_task',
  toolResult: { success: true },
  toolSuccess: true,
}

describe('ChatMessage', () => {
  it('user message renders with primary background', () => {
    render(<ChatMessage message={userMsg} />)
    expect(screen.getByText('Hello')).toBeInTheDocument()
    const bubble = screen.getByText('Hello').closest('.bg-primary')
    expect(bubble).toBeInTheDocument()
  })

  it('assistant message renders with bot icon', () => {
    render(<ChatMessage message={assistantMsg} />)
    expect(screen.getByText('Hi there!')).toBeInTheDocument()
  })

  it('status message renders as centered pill with info content', () => {
    render(<ChatMessage message={statusMsg} />)
    expect(screen.getByText(/Connected to anthropic/)).toBeInTheDocument()
  })

  it('error message renders as centered pill with alert content', () => {
    render(<ChatMessage message={errorMsg} />)
    expect(screen.getByText('Connection failed')).toBeInTheDocument()
    const pill = screen.getByText('Connection failed').closest('.bg-destructive\\/10')
    expect(pill).toBeInTheDocument()
  })

  it('tool call messages delegate to ChatToolCall', () => {
    render(<ChatMessage message={toolCallMsg} />)
    expect(screen.getByText(/→/)).toBeInTheDocument()
    expect(screen.getByText(/create_task/)).toBeInTheDocument()
  })

  it('tool result messages delegate to ChatToolCall', () => {
    render(<ChatMessage message={toolResultMsg} />)
    expect(screen.getByText(/✓/)).toBeInTheDocument()
    expect(screen.getByText(/create_task/)).toBeInTheDocument()
  })

  it('usage metadata is shown when present on assistant messages', () => {
    render(<ChatMessage message={assistantMsg} />)
    expect(screen.getByText('1 LLM call')).toBeInTheDocument()
    expect(screen.getByText('2 tool calls')).toBeInTheDocument()
    expect(screen.getByText('1.5s')).toBeInTheDocument()
  })

  it('pluralizes usage counts', () => {
    render(<ChatMessage message={{ ...assistantMsg, usage: { llmCalls: 2, toolCalls: 1, tasksCreated: 1 } }} />)
    expect(screen.getByText('2 LLM calls')).toBeInTheDocument()
    expect(screen.getByText('1 tool call')).toBeInTheDocument()
    expect(screen.getByText('1 task')).toBeInTheDocument()
  })

  it('wraps long unbroken text inside the bubbles', () => {
    const long = 'x'.repeat(400)
    const { rerender } = render(<ChatMessage message={{ ...userMsg, content: long }} />)
    expect(screen.getByText(long).className).toContain('wrap-anywhere')
    rerender(<ChatMessage message={{ ...assistantMsg, content: long }} />)
    expect(screen.getByText(long).className).toContain('wrap-anywhere')
  })

  it('wraps long status and error pills instead of widening the transcript', () => {
    const model = `Connected to vekil/${'m'.repeat(200)}`
    const body = `Error 502: {"error":{"message":"${'x'.repeat(300)}"}}`
    const expectWrappingPill = (content: string) => {
      const text = screen.getByText(content)
      expect(text.className).toContain('wrap-anywhere')
      expect(text.className).toContain('min-w-0')
      const pill = text.parentElement as HTMLElement
      expect(pill.className).toContain('max-w-full')
      expect(pill.className).toContain('min-w-0')
      // A fully rounded box would clip the corners of a wrapped line.
      expect(pill.className).not.toMatch(/(^|\s)rounded-full(\s|$)/)
    }
    const { rerender } = render(<ChatMessage message={{ ...statusMsg, content: model }} />)
    expectWrappingPill(model)
    rerender(<ChatMessage message={{ ...errorMsg, content: body }} />)
    expectWrappingPill(body)
  })

  it('task chips show the live task phase and stop polling once terminal', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      let requests = 0
      server.use(http.get('/api/v1/tasks/:id', ({ params }) => {
        requests += 1
        return HttpResponse.json({
          metadata: { name: params.id, namespace: 'default' },
          spec: { type: 'ai' },
          status: { phase: requests === 1 ? 'Running' : 'Succeeded' },
        })
      }))
      render(<ChatMessage message={{ ...assistantMsg, tasksCreatedNames: ['task-live'] }} />)
      const chip = screen.getByRole('link', { name: /task-live/ })
      await waitFor(() => expect(chip).toHaveTextContent('Running'))
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000)
      })
      await waitFor(() => expect(chip).toHaveTextContent('Succeeded'))
      const settled = requests
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000)
      })
      expect(requests).toBe(settled)
    } finally {
      vi.useRealTimers()
    }
  })

  it('renders clickable task chips when the turn created tasks', () => {
    const msg: ChatMessageType = {
      id: 'msg-7',
      role: 'assistant',
      content: 'Created two tasks.',
      timestamp: new Date().toISOString(),
      usage: { tasksCreated: 2 },
      tasksCreatedNames: ['task-alpha', 'task-beta'],
    }
    render(<ChatMessage message={msg} />)
    const alpha = screen.getByRole('link', { name: /task-alpha/ })
    const beta = screen.getByRole('link', { name: /task-beta/ })
    expect(alpha).toHaveAttribute('href', '/tasks/$taskId')
    expect(beta).toBeInTheDocument()
  })

  it('renders no task chips when none were created', () => {
    render(<ChatMessage message={assistantMsg} />)
    expect(screen.queryByRole('link')).not.toBeInTheDocument()
  })
})
