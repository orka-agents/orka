import { useState } from 'react'
import { cn } from '@/lib/utils'
import { ChevronRight } from 'lucide-react'
import type { ChatMessage } from '@/schemas/chat'

export function ChatToolCall({ message }: { message: ChatMessage }) {
  const [expanded, setExpanded] = useState(false)
  const isResult = message.role === 'tool_result'
  const success = message.toolSuccess

  // A single status-dot color encodes the tool call's state, drawn from the
  // shared status tokens (call = info/running, ok = succeeded, fail = failed).
  const dotClass = isResult
    ? success
      ? 'bg-status-succeeded'
      : 'bg-status-failed'
    : 'bg-status-running'

  const glyph = isResult ? (success ? '✓' : '✗') : '→'

  return (
    // The indent lines up with message text from sm up; phones keep the width.
    <div className="my-1 sm:mx-12">
      <button
        onClick={() => setExpanded(!expanded)}
        aria-expanded={expanded}
        className={cn(
          'flex w-full items-center gap-2 rounded-md border border-border bg-card px-3 py-1.5 text-left text-xs transition-colors hover:bg-accent',
        )}
      >
        <span className={cn('inline-block size-1.5 shrink-0 rounded-full', dotClass)} aria-hidden="true" />
        <span className="min-w-0 truncate font-mono font-medium text-foreground" title={message.toolName}>
          {glyph} {message.toolName}
        </span>
        <ChevronRight
          className={cn('ml-auto size-3 text-muted-foreground transition-transform', expanded && 'rotate-90')}
          aria-hidden="true"
        />
      </button>
      {expanded && (
        <div className="mt-1 rounded-md border border-border bg-muted/50 p-2">
          {!isResult && message.toolArgs != null && (
            <div>
              <span className="text-[10px] font-medium uppercase text-muted-foreground">Args</span>
              <pre className="mt-0.5 overflow-x-auto whitespace-pre-wrap wrap-anywhere text-xs font-mono text-foreground">
                {JSON.stringify(message.toolArgs, null, 2)}
              </pre>
            </div>
          )}
          {isResult && message.toolResult != null && (
            <div>
              <span className="text-[10px] font-medium uppercase text-muted-foreground">Result</span>
              <pre
                tabIndex={0}
                role="region"
                aria-label={`${message.toolName ?? 'Tool'} result`}
                className="mt-0.5 max-h-64 overflow-auto whitespace-pre-wrap wrap-anywhere text-xs font-mono text-foreground"
              >
                {JSON.stringify(message.toolResult, null, 2)}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  )
}
