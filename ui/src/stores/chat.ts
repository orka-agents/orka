import { create } from 'zustand'
import type { ChatMessage, ChatUsage } from '@/schemas/chat'

interface ChatState {
  messages: ChatMessage[]
  currentSessionId: string | null
  isStreaming: boolean
  // Actions
  addMessage: (message: ChatMessage) => void
  updateLastAssistantMessage: (content: string) => void
  setSessionId: (id: string) => void
  setStreaming: (streaming: boolean) => void
  newSession: () => void
  setUsageOnLastAssistant: (usage: ChatUsage) => void
}

let msgCounter = 0
export function generateMessageId(): string {
  return `msg-${Date.now()}-${++msgCounter}`
}

export const useChatStore = create<ChatState>()((set) => ({
  messages: [],
  currentSessionId: null,
  isStreaming: false,

  addMessage: (message) =>
    set((state) => ({ messages: [...state.messages, message] })),

  updateLastAssistantMessage: (content) =>
    set((state) => {
      const msgs = [...state.messages]
      for (let i = msgs.length - 1; i >= 0; i--) {
        if (msgs[i].role === 'assistant') {
          msgs[i] = { ...msgs[i], content }
          break
        }
      }
      return { messages: msgs }
    }),

  setSessionId: (id) => set({ currentSessionId: id }),
  setStreaming: (streaming) => set({ isStreaming: streaming }),
  newSession: () => set({ messages: [], currentSessionId: null }),

  setUsageOnLastAssistant: (usage) =>
    set((state) => {
      const msgs = [...state.messages]
      for (let i = msgs.length - 1; i >= 0; i--) {
        if (msgs[i].role === 'assistant') {
          msgs[i] = { ...msgs[i], usage }
          break
        }
      }
      return { messages: msgs }
    }),
}))
