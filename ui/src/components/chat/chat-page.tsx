import { ChatMessageList } from './chat-message-list'
import { ChatInput } from './chat-input'
import { useSendMessage, useChatConfig } from '@/hooks/use-chat'
import { useChatStore } from '@/stores/chat'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Plus } from 'lucide-react'

export function ChatPage() {
  const sendMessage = useSendMessage()
  const { data: config } = useChatConfig()
  const { currentSessionId, newSession, messages } = useChatStore()

  return (
    <div className="flex h-full flex-col -m-6">
      {/* Header */}
      <div className="flex items-center justify-between border-b border-border bg-card px-4 py-2">
        <div className="flex items-center gap-3">
          <h1 className="text-sm font-semibold">Chat</h1>
          {currentSessionId && (
            <Badge variant="secondary" className="font-mono text-[10px]">
              {currentSessionId}
            </Badge>
          )}
          {config && (
            <Badge variant="outline" className="text-[10px]">
              {config.model}
            </Badge>
          )}
        </div>
        <div className="flex items-center gap-2">
          {messages.length > 0 && (
            <Button variant="ghost" size="sm" onClick={newSession} className="h-7 text-xs">
              <Plus className="mr-1 h-3 w-3" /> New Chat
            </Button>
          )}
        </div>
      </div>

      {/* Messages */}
      <ChatMessageList />

      {/* Input */}
      <ChatInput onSend={sendMessage} />
    </div>
  )
}
