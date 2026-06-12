import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

import { render, screen, fireEvent } from '@/test/test-utils'
import { ChatToolCall } from './chat-tool-call'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useChatStore } from '@/stores/chat'
import type { ChatMessage } from '@/schemas/chat'

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false })
})

const toolCallMsg: ChatMessage = {
  id: 'msg-5',
  role: 'tool_call',
  content: 'create_task',
  timestamp: new Date().toISOString(),
  toolCallId: 'tc-1',
  toolName: 'create_task',
  toolArgs: { name: 'test' },
}

const toolResultSuccess: ChatMessage = {
  id: 'msg-6',
  role: 'tool_result',
  content: 'create_task',
  timestamp: new Date().toISOString(),
  toolCallId: 'tc-1',
  toolName: 'create_task',
  toolResult: { success: true },
  toolSuccess: true,
}

const toolResultFailure: ChatMessage = {
  id: 'msg-7',
  role: 'tool_result',
  content: 'create_task',
  timestamp: new Date().toISOString(),
  toolCallId: 'tc-2',
  toolName: 'create_task',
  toolResult: { success: false, error: 'failed' },
  toolSuccess: false,
}

describe('ChatToolCall', () => {
  it('tool call shows arrow icon and tool name', () => {
    render(<ChatToolCall message={toolCallMsg} />)
    expect(screen.getByText(/→/)).toBeInTheDocument()
    expect(screen.getByText(/create_task/)).toBeInTheDocument()
  })

  it('tool result with success shows checkmark', () => {
    render(<ChatToolCall message={toolResultSuccess} />)
    expect(screen.getByText(/✓/)).toBeInTheDocument()
  })

  it('tool result with failure shows X mark', () => {
    render(<ChatToolCall message={toolResultFailure} />)
    expect(screen.getByText(/✗/)).toBeInTheDocument()
  })

  it('clicking toggles expanded state', () => {
    render(<ChatToolCall message={toolCallMsg} />)
    const button = screen.getByRole('button')
    // Collapsed by default; the Args body is not shown.
    expect(button).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('Args')).not.toBeInTheDocument()
    fireEvent.click(button)
    expect(button).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('Args')).toBeInTheDocument()
    fireEvent.click(button)
    expect(button).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('Args')).not.toBeInTheDocument()
  })

  it('expanded state shows args for tool_call', () => {
    render(<ChatToolCall message={toolCallMsg} />)
    fireEvent.click(screen.getByRole('button'))
    expect(screen.getByText('Args')).toBeInTheDocument()
    expect(screen.getByText(/"name": "test"/)).toBeInTheDocument()
  })

  it('expanded state shows result for tool_result', () => {
    render(<ChatToolCall message={toolResultSuccess} />)
    fireEvent.click(screen.getByRole('button'))
    expect(screen.getByText('Result')).toBeInTheDocument()
    expect(screen.getByText(/"success": true/)).toBeInTheDocument()
  })
})
