import { describe, it, expect, vi, beforeEach } from 'vitest'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

import { act } from 'react'
import { render, screen, fireEvent } from '@/test/test-utils'
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
  it('empty state shows welcome text with orca brand mark', () => {
    render(<ChatMessageList />)
    // The page title is the h1, so the welcome heading is the next level down.
    expect(screen.getByRole('heading', { level: 2, name: 'Orka Meta Agent' })).toBeInTheDocument()
    expect(
      screen.getByText(/Chat with the orchestrator/i),
    ).toBeInTheDocument()
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

  it('scrolls its own viewport and follows the stream only near the bottom', () => {
    const now = new Date().toISOString()
    useChatStore.setState({ messages: [{ id: 'm1', role: 'user', content: 'Hello', timestamp: now }] })
    const { container } = render(<ChatMessageList />)
    const root = container.querySelector('[data-slot="scroll-area"]') as HTMLElement
    // The list is a shrinkable flex child, so it scrolls instead of the page.
    expect(root.className).toContain('min-h-0')
    expect(root.className).toContain('flex-1')
    const viewport = container.querySelector('[data-slot="scroll-area-viewport"]') as HTMLElement
    // Radix's content wrapper is a `display: table` box that grows to the
    // widest unbreakable line; forcing it to a block keeps the transcript
    // inside the viewport.
    expect(viewport).toHaveClass('[&>div]:block!')
    let top = 0
    Object.defineProperty(viewport, 'scrollHeight', { configurable: true, get: () => 1000 })
    Object.defineProperty(viewport, 'clientHeight', { configurable: true, get: () => 200 })
    Object.defineProperty(viewport, 'scrollTop', { configurable: true, get: () => top, set: (value: number) => { top = value } })
    const add = (message: ChatMessage) => act(() => useChatStore.getState().addMessage(message))

    // A reader who scrolled up keeps their place while the reply grows.
    top = 100
    fireEvent.scroll(viewport)
    add({ id: 'm2', role: 'assistant', content: 'part one', timestamp: now })
    expect(top).toBe(100)

    // Back near the bottom, new content is followed.
    top = 760
    fireEvent.scroll(viewport)
    add({ id: 'm3', role: 'assistant', content: 'part two', timestamp: now })
    expect(top).toBe(1000)

    // The reader's own message always returns to the bottom.
    top = 0
    fireEvent.scroll(viewport)
    add({ id: 'm4', role: 'user', content: 'again', timestamp: now })
    expect(top).toBe(1000)
    expect(Element.prototype.scrollIntoView).not.toHaveBeenCalled()
  })
})
