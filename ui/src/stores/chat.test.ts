import { describe, it, expect, beforeEach } from 'vitest'
import { useChatStore, generateMessageId } from './chat'
import type { ChatMessage, ChatUsage } from '@/schemas/chat'

function makeMessage(overrides: Partial<ChatMessage> & { role: ChatMessage['role'] }): ChatMessage {
  return {
    id: generateMessageId(),
    content: 'hello',
    timestamp: new Date().toISOString(),
    ...overrides,
  }
}

describe('useChatStore', () => {
  beforeEach(() => {
    useChatStore.setState({ messages: [], currentSessionId: null, isStreaming: false })
  })

  it('has correct initial state', () => {
    const state = useChatStore.getState()
    expect(state.messages).toEqual([])
    expect(state.currentSessionId).toBeNull()
    expect(state.isStreaming).toBe(false)
  })

  it('addMessage appends a message', () => {
    const msg = makeMessage({ role: 'user', content: 'hi' })
    useChatStore.getState().addMessage(msg)
    expect(useChatStore.getState().messages).toHaveLength(1)
    expect(useChatStore.getState().messages[0]).toEqual(msg)
  })

  it('updateLastAssistantMessage updates last assistant message content', () => {
    const userMsg = makeMessage({ role: 'user', content: 'question' })
    const assistantMsg = makeMessage({ role: 'assistant', content: 'original' })
    useChatStore.getState().addMessage(userMsg)
    useChatStore.getState().addMessage(assistantMsg)

    useChatStore.getState().updateLastAssistantMessage('updated')

    const msgs = useChatStore.getState().messages
    expect(msgs[1].content).toBe('updated')
    expect(msgs[0].content).toBe('question')
  })

  it('updateLastAssistantMessage does nothing with no assistant messages', () => {
    const userMsg = makeMessage({ role: 'user', content: 'hi' })
    useChatStore.getState().addMessage(userMsg)
    useChatStore.getState().updateLastAssistantMessage('nope')

    expect(useChatStore.getState().messages).toHaveLength(1)
    expect(useChatStore.getState().messages[0].content).toBe('hi')
  })

  it('updateLastAssistantMessage updates only the last assistant message', () => {
    useChatStore.getState().addMessage(makeMessage({ role: 'assistant', content: 'first' }))
    useChatStore.getState().addMessage(makeMessage({ role: 'user', content: 'q' }))
    useChatStore.getState().addMessage(makeMessage({ role: 'assistant', content: 'second' }))

    useChatStore.getState().updateLastAssistantMessage('changed')

    const msgs = useChatStore.getState().messages
    expect(msgs[0].content).toBe('first')
    expect(msgs[2].content).toBe('changed')
  })

  it('setSessionId updates sessionId', () => {
    useChatStore.getState().setSessionId('sess-1')
    expect(useChatStore.getState().currentSessionId).toBe('sess-1')
  })

  it('setStreaming updates streaming state', () => {
    useChatStore.getState().setStreaming(true)
    expect(useChatStore.getState().isStreaming).toBe(true)
    useChatStore.getState().setStreaming(false)
    expect(useChatStore.getState().isStreaming).toBe(false)
  })

  it('newSession clears messages and sessionId', () => {
    useChatStore.getState().addMessage(makeMessage({ role: 'user', content: 'hi' }))
    useChatStore.getState().setSessionId('sess-1')
    useChatStore.getState().newSession()

    expect(useChatStore.getState().messages).toEqual([])
    expect(useChatStore.getState().currentSessionId).toBeNull()
  })

  it('setUsageOnLastAssistant sets usage on last assistant message', () => {
    useChatStore.getState().addMessage(makeMessage({ role: 'assistant', content: 'answer' }))
    const usage: ChatUsage = { inputTokens: 10, outputTokens: 20 }
    useChatStore.getState().setUsageOnLastAssistant(usage)

    expect(useChatStore.getState().messages[0].usage).toEqual(usage)
  })

  it('setUsageOnLastAssistant with no assistant messages does nothing', () => {
    useChatStore.getState().addMessage(makeMessage({ role: 'user', content: 'hi' }))
    const usage: ChatUsage = { inputTokens: 5 }
    useChatStore.getState().setUsageOnLastAssistant(usage)

    expect(useChatStore.getState().messages[0].usage).toBeUndefined()
  })
})

describe('generateMessageId', () => {
  it('returns unique IDs', () => {
    const ids = new Set([generateMessageId(), generateMessageId(), generateMessageId()])
    expect(ids.size).toBe(3)
  })

  it('returns string starting with msg-', () => {
    expect(generateMessageId()).toMatch(/^msg-/)
  })
})
