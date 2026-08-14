import { ChatMessageList } from './chat-message-list'
import { ChatInput } from './chat-input'
import { useCancelChatSession, useSendMessage, useChatConfig } from '@/hooks/use-chat'
import { useChatStore } from '@/stores/chat'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Plus } from 'lucide-react'
import { toast } from 'sonner'

export function ChatPage() {
  const sendMessage = useSendMessage()
  const { data: config } = useChatConfig()
  const { currentSessionId, isStreaming, newSession, messages } = useChatStore()
  const cancelSession = useCancelChatSession()

  // Starting over also cancels any in-flight turn and deletes the session
  // server-side, so abandoned chats do not keep consuming the concurrency
  // budget. Local reset happens regardless of the server outcome.
  const handleNewChat = async () => {
    const sessionId = currentSessionId
    newSession()
    if (!sessionId) return
    try {
      await cancelSession.mutateAsync(sessionId)
    } catch (error) {
      const message = error instanceof Error ? error.message : 'unknown error'
      toast.error(`Previous chat session was not removed: ${message}`)
    }
  }

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
          {isStreaming && (
            <Badge variant="outline" className="text-[10px]">
              streaming
            </Badge>
          )}
        </div>
        <div className="flex items-center gap-2">
          {messages.length > 0 && (
            <Button variant="ghost" size="sm" onClick={handleNewChat} className="h-7 text-xs">
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
