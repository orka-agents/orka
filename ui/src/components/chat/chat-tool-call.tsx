import { useState } from 'react'
import { cn } from '@/lib/utils'
import type { ChatMessage } from '@/schemas/chat'

export function ChatToolCall({ message }: { message: ChatMessage }) {
  const [expanded, setExpanded] = useState(false)
  const isResult = message.role === 'tool_result'
  const success = message.toolSuccess

  return (
    <div className="mx-12 my-1">
      <button
        onClick={() => setExpanded(!expanded)}
        className={cn(
          'flex w-full items-center gap-2 rounded-md border px-3 py-1.5 text-left text-xs transition-colors',
          isResult
            ? success
              ? 'border-green-200 bg-green-50 text-green-800 dark:border-green-800 dark:bg-green-950 dark:text-green-200'
              : 'border-red-200 bg-red-50 text-red-800 dark:border-red-800 dark:bg-red-950 dark:text-red-200'
            : 'border-blue-200 bg-blue-50 text-blue-800 dark:border-blue-800 dark:bg-blue-950 dark:text-blue-200',
        )}
      >
        <span className="font-mono font-medium">
          {isResult ? (success ? '✓' : '✗') : '→'} {message.toolName}
        </span>
        <span className="ml-auto text-[10px] opacity-60">{expanded ? '▲' : '▼'}</span>
      </button>
      {expanded && (
        <div className="mt-1 rounded-md border border-border bg-muted/50 p-2">
          {!isResult && message.toolArgs != null && (
            <div>
              <span className="text-[10px] font-medium uppercase text-muted-foreground">Args</span>
              <pre className="mt-0.5 overflow-x-auto whitespace-pre-wrap text-xs font-mono text-foreground">
                {JSON.stringify(message.toolArgs, null, 2)}
              </pre>
            </div>
          )}
          {isResult && message.toolResult != null && (
            <div>
              <span className="text-[10px] font-medium uppercase text-muted-foreground">Result</span>
              <pre className="mt-0.5 max-h-64 overflow-auto whitespace-pre-wrap text-xs font-mono text-foreground">
                {JSON.stringify(message.toolResult, null, 2)}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  )
}
