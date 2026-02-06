import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

import { render, screen } from '@/test/test-utils'
import { ChatMessageList } from './chat-message-list'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { useChatStore } from '@/stores/chat'
import type { ChatMessage } from '@/schemas/chat'

beforeEach(() => {
  useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  useAuthStore.setState({ token: 'test-token' })
  useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false })
  Element.prototype.scrollIntoView = vi.fn()
})

describe('ChatMessageList', () => {
  it('empty state shows welcome text with fish emoji', () => {
    render(<ChatMessageList />)
    expect(screen.getByText('🪸')).toBeInTheDocument()
    expect(screen.getByText('Mercan Meta Agent')).toBeInTheDocument()
  })

  it('with messages in store, renders ChatMessage for each', () => {
    const msgs: ChatMessage[] = [
      { id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() },
      { id: 'msg-2', role: 'assistant', content: 'Hi there!', timestamp: new Date().toISOString() },
    ]
    useChatStore.setState({ messages: msgs })
    render(<ChatMessageList />)
    expect(screen.getByText('Hello')).toBeInTheDocument()
    expect(screen.getByText('Hi there!')).toBeInTheDocument()
  })

  it('streaming indicator shown when isStreaming is true', () => {
    const msgs: ChatMessage[] = [
      { id: 'msg-1', role: 'user', content: 'Hello', timestamp: new Date().toISOString() },
    ]
    useChatStore.setState({ messages: msgs, isStreaming: true })
    render(<ChatMessageList />)
    expect(screen.getByText('Thinking...')).toBeInTheDocument()
  })
})
