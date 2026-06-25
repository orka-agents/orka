import { Link } from '@tanstack/react-router'
import { Bot, User, AlertCircle, Info } from 'lucide-react'
import { ChatToolCall } from './chat-tool-call'
import { StatusDot } from '@/components/ui/status-dot'
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

  // User turns keep a right-aligned bubble; assistant turns render flush (like
  // an agentic console, not a symmetric consumer chat).
  if (isUser) {
    return (
      <div className="flex justify-end gap-3 py-2">
        <div className="max-w-[80%] rounded-2xl bg-primary px-4 py-2.5 text-sm text-primary-foreground">
          <div className="whitespace-pre-wrap">{message.content}</div>
        </div>
        <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-secondary">
          <User className="h-4 w-4" />
        </div>
      </div>
    )
  }

  return (
    <div className="flex gap-3 py-2">
      <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-primary/10">
        <Bot className="h-4 w-4 text-primary" />
      </div>
      <div className="min-w-0 flex-1 space-y-1.5">
        <div className="whitespace-pre-wrap break-words text-sm text-foreground [overflow-wrap:anywhere]">{message.content}</div>

        {/* Cross-silo links: tasks created during this turn link into the graph. */}
        {message.tasksCreatedNames && message.tasksCreatedNames.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {message.tasksCreatedNames.map((name) => (
              <Link
                key={name}
                to="/tasks/$taskId"
                params={{ taskId: name }}
                className="inline-flex items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs font-mono transition-colors hover:bg-accent"
              >
                <StatusDot phase="Pending" hideLabel />
                {name}
              </Link>
            ))}
          </div>
        )}

        {message.usage && (
          <div className="flex flex-wrap gap-3 text-[10px] text-muted-foreground tabular-nums">
            {message.usage.llmCalls !== undefined && <span>{message.usage.llmCalls} LLM calls</span>}
            {message.usage.toolCalls !== undefined && <span>{message.usage.toolCalls} tool calls</span>}
            {message.usage.tasksCreated !== undefined && message.usage.tasksCreated > 0 && (
              <span>{message.usage.tasksCreated} tasks</span>
            )}
            {message.usage.duration && <span>{message.usage.duration}</span>}
          </div>
        )}
      </div>
    </div>
  )
}
