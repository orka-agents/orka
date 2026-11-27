import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Bot, User, AlertCircle, Info } from 'lucide-react'
import { ChatToolCall } from './chat-tool-call'
import { StatusDot } from '@/components/ui/status-dot'
import { useTask } from '@/hooks/use-tasks'
import type { ChatMessage as ChatMessageType } from '@/schemas/chat'
import { isTaskTerminal } from '@/lib/task-phase'

const TASK_CHIP_POLL_MS = 10_000

function plural(count: number, singular: string, pluralForm = `${singular}s`) {
  return `${count} ${count === 1 ? singular : pluralForm}`
}

function TaskChip({ name }: { name: string }) {
  const [polling, setPolling] = useState(true)
  const { data: task } = useTask(name, polling ? TASK_CHIP_POLL_MS : false)
  const phase = task?.status?.phase
  if (polling && phase && isTaskTerminal(phase)) setPolling(false)

  return (
    <Link
      to="/tasks/$taskId"
      params={{ taskId: name }}
      className="inline-flex max-w-full items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs font-mono transition-colors hover:bg-accent"
    >
      <StatusDot phase={phase ?? 'Pending'} hideLabel />
      <span className="truncate">{name}</span>
    </Link>
  )
}

export function ChatMessage({ message }: { message: ChatMessageType }) {
  if (message.role === 'tool_call' || message.role === 'tool_result') {
    return <ChatToolCall message={message} />
  }

  if (message.role === 'status') {
    return (
      <div className="flex justify-center py-1">
        <span className="flex min-w-0 max-w-full items-center gap-1.5 rounded-2xl bg-muted px-3 py-1 text-xs text-muted-foreground">
          <Info className="h-3 w-3 shrink-0" />
          <span className="min-w-0 wrap-anywhere">{message.content}</span>
        </span>
      </div>
    )
  }

  if (message.role === 'error') {
    return (
      <div className="flex justify-center py-1">
        <span className="flex min-w-0 max-w-full items-center gap-1.5 rounded-2xl bg-destructive/10 px-3 py-1 text-xs text-destructive">
          <AlertCircle className="h-3 w-3 shrink-0" />
          <span className="min-w-0 wrap-anywhere">{message.content}</span>
        </span>
      </div>
    )
  }

  const isUser = message.role === 'user'

  if (isUser) {
    return (
      <div className="flex justify-end gap-3 py-2">
        <div className="max-w-[80%] rounded-2xl bg-primary px-4 py-2.5 text-sm text-primary-foreground">
          <div className="whitespace-pre-wrap wrap-anywhere">{message.content}</div>
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
        <div className="whitespace-pre-wrap wrap-anywhere text-sm text-foreground">{message.content}</div>

        {message.tasksCreatedNames && message.tasksCreatedNames.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {message.tasksCreatedNames.map((name) => (
              <TaskChip key={name} name={name} />
            ))}
          </div>
        )}

        {message.usage && (
          <div className="flex flex-wrap gap-3 text-[10px] text-muted-foreground tabular-nums">
            {message.usage.llmCalls !== undefined && <span>{plural(message.usage.llmCalls, 'LLM call')}</span>}
            {message.usage.toolCalls !== undefined && <span>{plural(message.usage.toolCalls, 'tool call')}</span>}
            {message.usage.tasksCreated !== undefined && message.usage.tasksCreated > 0 && (
              <span>{plural(message.usage.tasksCreated, 'task')}</span>
            )}
            {message.usage.duration && <span>{message.usage.duration}</span>}
          </div>
        )}
      </div>
    </div>
  )
}
