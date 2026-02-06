import { Card, CardContent } from '@/components/ui/card'
import { Bot, User } from 'lucide-react'
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

export function TranscriptViewer({ transcript }: { transcript?: string }) {
  const messages = parseTranscript(transcript)

  if (messages.length === 0) {
    return <p className="text-sm text-muted-foreground py-4">No messages in this session.</p>
  }

  return (
    <div className="space-y-4">
      {messages.map((msg, i) => (
        <div key={i} className={cn('flex gap-3', msg.role === 'user' ? 'justify-end' : 'justify-start')}>
          {msg.role !== 'user' && (
            <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-primary/10">
              <Bot className="h-4 w-4 text-primary" />
            </div>
          )}
          <Card className={cn('max-w-[80%]', msg.role === 'user' ? 'bg-primary text-primary-foreground' : 'bg-card')}>
            <CardContent className="p-3">
              <pre className="whitespace-pre-wrap text-sm font-sans">{msg.content}</pre>
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
      ))}
    </div>
  )
}
