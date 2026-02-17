import { describe, it, expect, vi, beforeEach } from 'vitest'

const { mockUseChatConfig, mockSendMessage } = vi.hoisted(() => ({
  mockUseChatConfig: vi.fn(),
  mockSendMessage: vi.fn(),
}))

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

vi.mock('@/hooks/use-chat', () => ({
  useSendMessage: () => mockSendMessage,
  useChatConfig: () => mockUseChatConfig(),
}))

import { render, screen } from '@/test/test-utils'
import { ChatPage } from './chat-page'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useChatStore } from '@/stores/chat'
import type { ChatMessage } from '@/schemas/chat'

beforeEach(() => {
  mockSendMessage.mockReset()
  mockUseChatConfig.mockReset()
  mockUseChatConfig.mockReturnValue({ data: { model: 'claude-sonnet-4-20250514', enabled: true } })
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false })
  Element.prototype.scrollIntoView = vi.fn()
})

describe('ChatPage', () => {
  it('renders "Chat" heading', () => {
    render(<ChatPage />)
    expect(screen.getByText('Chat')).toBeInTheDocument()
  })

  it('renders without crashing', () => {
    const { container } = render(<ChatPage />)
    expect(container).toBeTruthy()
  })

  it('New Chat button appears when messages exist', () => {
    const msgs: ChatMessage[] = [
      { id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() },
    ]
    useChatStore.setState({ messages: msgs })
    render(<ChatPage />)
    expect(screen.getByText('New Chat')).toBeInTheDocument()
  })

  it('session ID badge shown when currentSessionId is set', () => {
    useChatStore.setState({ currentSessionId: 'session-abc-123' })
    render(<ChatPage />)
    expect(screen.getByText('session-abc-123')).toBeInTheDocument()
  })

  it('shows config error badge when chat config fails to load', () => {
    mockUseChatConfig.mockReturnValue({ data: undefined, isError: true, error: new Error('boom') })
    render(<ChatPage />)
    expect(screen.getByText('Failed to load')).toBeInTheDocument()
  })
})
