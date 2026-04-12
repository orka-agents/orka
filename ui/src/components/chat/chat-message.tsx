import { Bot, User, AlertCircle, Info } from 'lucide-react'
import { cn } from '@/lib/utils'
import { ChatToolCall } from './chat-tool-call'
import type { ChatMessage as ChatMessageType } from '@/schemas/chat'

export function ChatMessage({ message }: { message: ChatMessageType }) {
  // Tool call/result messages get their own compact display
  if (message.role === 'tool_call' || message.role === 'tool_result') {
    return <ChatToolCall message={message} />
  }

  // Status messages
  if (message.role === 'status') {
    return (
      <div className="flex justify-center py-1">
        <span className="flex items-center gap-1.5 rounded-full bg-muted px-3 py-1 text-xs text-muted-foreground">
          <Info className="h-3 w-3" />
          {message.content}
        </span>
      </div>
    )
  }

  // Error messages
  if (message.role === 'error') {
    return (
      <div className="flex justify-center py-1">
        <span className="flex items-center gap-1.5 rounded-full bg-destructive/10 px-3 py-1 text-xs text-destructive">
          <AlertCircle className="h-3 w-3" />
          {message.content}
        </span>
      </div>
    )
  }

  const isUser = message.role === 'user'

  return (
    <div className={cn('flex gap-3 py-2', isUser ? 'justify-end' : 'justify-start')}>
      {!isUser && (
        <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-primary/10">
          <Bot className="h-4 w-4 text-primary" />
        </div>
      )}
      <div className={cn('max-w-[80%] space-y-1')}>
        <div
          className={cn(
            'rounded-2xl px-4 py-2.5 text-sm',
            isUser
              ? 'bg-primary text-primary-foreground'
              : 'bg-muted text-foreground',
          )}
        >
          <div className="whitespace-pre-wrap">{message.content}</div>
        </div>
        {message.usage && (
          <div className="flex gap-3 px-2 text-[10px] text-muted-foreground">
            {message.usage.llmCalls !== undefined && <span>{message.usage.llmCalls} LLM calls</span>}
            {message.usage.toolCalls !== undefined && <span>{message.usage.toolCalls} tool calls</span>}
            {message.usage.tasksCreated !== undefined && message.usage.tasksCreated > 0 && (
              <span>{message.usage.tasksCreated} tasks</span>
            )}
            {message.usage.duration && <span>{message.usage.duration}</span>}
          </div>
        )}
      </div>
      {isUser && (
        <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-secondary">
          <User className="h-4 w-4" />
        </div>
      )}
    </div>
  )
}
