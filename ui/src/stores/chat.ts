import { create } from 'zustand'
import { persist, type PersistStorage, type StorageValue } from 'zustand/middleware'
import type { ChatMessage, ChatUsage } from '@/schemas/chat'

interface ChatState {
  messages: ChatMessage[]
  currentSessionId: string | null
  isStreaming: boolean
  stopRequested: boolean
  provider: string
  model: string
  activeNamespace: string
  selections: Record<string, ChatSelection>
  turnEpoch: number
  setProvider: (provider: string) => void
  setModel: (model: string) => void
  selectNamespace: (namespace: string) => void
  addMessage: (message: ChatMessage) => void
  setSessionId: (id: string | null) => void
  setStreaming: (streaming: boolean) => void
  requestStop: () => void
  newSession: () => void
  resetForIdentityChange: () => void
  setUsageOnLastAssistant: (usage: ChatUsage, tasksCreatedNames?: string[]) => void
}

function rawStorage(): Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> {
  try {
    if (typeof window !== 'undefined' && window.localStorage) return window.localStorage
  } catch {
  }
  const memory = new Map<string, string>()
  return {
    getItem: (key) => memory.get(key) ?? null,
    setItem: (key, value) => {
      memory.set(key, value)
    },
    removeItem: (key) => {
      memory.delete(key)
    },
  }
}

interface ChatSelection {
  provider: string
  model: string
}

type PersistedChatState = Pick<ChatState, 'selections'>

function chatStorage(): PersistStorage<PersistedChatState> {
  const raw = rawStorage()
  return {
    getItem: (name) => {
      const value = raw.getItem(name)
      if (!value) return null
      try {
        return JSON.parse(value) as StorageValue<PersistedChatState>
      } catch {
        return null
      }
    },
    setItem: (name, value) => raw.setItem(name, JSON.stringify(value)),
    removeItem: (name) => raw.removeItem(name),
  }
}

let msgCounter = 0
export function generateMessageId(): string {
  return `msg-${Date.now()}-${++msgCounter}`
}

export const useChatStore = create<ChatState>()(
  persist(
    (set) => ({
  messages: [],
  currentSessionId: null,
  isStreaming: false,
  stopRequested: false,
  provider: '',
  model: '',
  activeNamespace: '',
  turnEpoch: 0,
  selections: {},

  setProvider: (provider) =>
    set((state) => ({
      provider,
      selections: { ...state.selections, [state.activeNamespace]: { provider, model: state.model } },
    })),
  setModel: (model) =>
    set((state) => ({
      model,
      selections: { ...state.selections, [state.activeNamespace]: { provider: state.provider, model } },
    })),
  selectNamespace: (namespace) =>
    set((state) => {
      const selection = state.selections[namespace]
      const switching = state.activeNamespace !== '' && state.activeNamespace !== namespace
      return {
        activeNamespace: namespace,
        provider: selection?.provider ?? '',
        model: selection?.model ?? '',
        ...(switching ? { messages: [], currentSessionId: null, isStreaming: false, stopRequested: false, turnEpoch: state.turnEpoch + 1 } : {}),
      }
    }),

  addMessage: (message) =>
    set((state) => ({ messages: [...state.messages, message] })),

  setSessionId: (id) => set({ currentSessionId: id }),
  setStreaming: (streaming) => set({ isStreaming: streaming, stopRequested: false }),
  requestStop: () => set((state) => (state.isStreaming ? { stopRequested: true } : {})),
  newSession: () => set((state) => ({ messages: [], currentSessionId: null, isStreaming: false, stopRequested: false, turnEpoch: state.turnEpoch + 1 })),

  resetForIdentityChange: () =>
    set((state) => ({
      messages: [],
      currentSessionId: null,
      isStreaming: false,
      stopRequested: false,
      provider: '',
      model: '',
      selections: {},
      turnEpoch: state.turnEpoch + 1,
    })),

  setUsageOnLastAssistant: (usage, tasksCreatedNames) =>
    set((state) => {
      const msgs = [...state.messages]
      for (let i = msgs.length - 1; i >= 0; i--) {
        if (msgs[i].role === 'assistant') {
          msgs[i] = {
            ...msgs[i],
            usage,
            ...(tasksCreatedNames && tasksCreatedNames.length > 0
              ? { tasksCreatedNames }
              : {}),
          }
          break
        }
      }
      return { messages: msgs }
    }),
    }),
    {
      name: 'orka-chat',
      version: 1,
      storage: chatStorage(),
      partialize: (state) => ({ selections: state.selections }),
      migrate: (persisted, version) => {
        const state = (persisted ?? {}) as Partial<PersistedChatState>
        return { selections: version >= 1 && state.selections ? state.selections : {} }
      },
    },
  ),
)
