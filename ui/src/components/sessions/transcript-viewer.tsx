import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Bot, MessageSquare, User, Wrench } from 'lucide-react'
import { cn } from '@/lib/utils'
import type { TranscriptMessage } from '@/schemas/session'

function parseTranscript(jsonl?: string): TranscriptMessage[] {
  if (!jsonl) return []
  return jsonl
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      try { return JSON.parse(line) }
      catch { return null }
    })
    .filter(Boolean) as TranscriptMessage[]
}

function formatToolContent(content: unknown): string {
  if (typeof content === 'string') return content
  return JSON.stringify(content, null, 2) ?? ''
}

function ToolDetails({ kind, name, id, content }: {
  kind: 'call' | 'result'
  name: string
  id?: string
  content: unknown
}) {
  return (
    <details className="rounded-md border border-border bg-muted/50 text-sm">
      <summary className="cursor-pointer px-3 py-2 font-medium">
        Tool {kind}: <span className="font-mono">{name}</span>
      </summary>
      <div className="space-y-2 border-t border-border p-3">
        {id && <div className="text-xs text-muted-foreground">Call ID: {id}</div>}
        <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words font-mono text-xs">
          {formatToolContent(content)}
        </pre>
      </div>
    </details>
  )
}

export function TranscriptViewer({ transcript }: { transcript?: string }) {
  const messages = parseTranscript(transcript)
  const toolNames = new Map<string, string>()

  if (messages.length === 0) {
    return <EmptyState icon={MessageSquare} headline="No messages in this session." />
  }

  return (
    <div className="space-y-4">
      {messages.map((msg, i) => {
        const toolCalls = Array.isArray(msg.toolCalls)
          ? msg.toolCalls.filter((call) => call && typeof call.id === 'string' && typeof call.name === 'string')
          : []
        for (const call of toolCalls) toolNames.set(call.id, call.name)
        const isTool = msg.role === 'tool'
        const Icon = isTool || (!msg.content && toolCalls.length > 0) ? Wrench : Bot

        return (
          <div key={i} className={cn('flex gap-3', msg.role === 'user' ? 'justify-end' : 'justify-start')}>
            {msg.role !== 'user' && (
              <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-primary/10">
                <Icon className="h-4 w-4 text-primary" />
              </div>
            )}
            <Card className={cn('min-w-0 max-w-[80%]', msg.role === 'user' ? 'bg-primary text-primary-foreground' : 'bg-card')}>
              <CardContent className="space-y-2 p-3">
                {isTool ? (
                  <ToolDetails
                    kind="result"
                    name={msg.name || toolNames.get(msg.toolCallID ?? '') || msg.toolCallID || 'Unknown tool'}
                    id={msg.toolCallID}
                    content={msg.content}
                  />
                ) : msg.content && (
                  <pre className="whitespace-pre-wrap text-sm font-sans">{msg.content}</pre>
                )}
                {toolCalls.map((call, callIndex) => (
                  <ToolDetails
                    key={callIndex}
                    kind="call"
                    name={call.name}
                    id={call.id}
                    content={typeof call.argumentsText === 'string' ? call.argumentsText : call.arguments}
                  />
                ))}
                {(msg.model || msg.inputTokens || msg.outputTokens) && (
                  <div className="mt-2 flex gap-2 text-xs opacity-70">
                    {msg.model && <span>{msg.model}</span>}
                    {msg.inputTokens !== undefined && <span>↑{msg.inputTokens}</span>}
                    {msg.outputTokens !== undefined && <span>↓{msg.outputTokens}</span>}
                  </div>
                )}
              </CardContent>
            </Card>
            {msg.role === 'user' && (
              <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-secondary">
                <User className="h-4 w-4" />
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
