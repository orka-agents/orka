import { useRef, useEffect, useState, useCallback } from 'react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ScrollArea } from '@/components/ui/scroll-area'
import { useTaskLogs } from '@/hooks/use-task-logs'
import { ArrowDown, Search, X, RefreshCw } from 'lucide-react'

type LogLevel = 'info' | 'warn' | 'error' | 'debug' | 'default'

const levelColors: Record<LogLevel, string> = {
  info: 'text-foreground',
  warn: 'text-yellow-600 dark:text-yellow-400',
  error: 'text-red-600 dark:text-red-400',
  debug: 'text-muted-foreground',
  default: 'text-foreground',
}

function parseLogLevel(line: string): LogLevel {
  const lower = line.toLowerCase()
  if (/\berror\b|"level"\s*:\s*"error"|level=error|\[error\]/i.test(lower)) return 'error'
  if (/\bwarn(ing)?\b|"level"\s*:\s*"warn"|level=warn|\[warn\]/i.test(lower)) return 'warn'
  if (/\bdebug\b|"level"\s*:\s*"debug"|level=debug|\[debug\]/i.test(lower)) return 'debug'
  if (/\binfo\b|"level"\s*:\s*"info"|level=info|\[info\]/i.test(lower)) return 'info'
  return 'default'
}

function HighlightedText({ text, search }: { text: string; search: string }) {
  if (!search) return <>{text}</>
  const parts = text.split(new RegExp(`(${search.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')})`, 'gi'))
  return (
    <>
      {parts.map((part, i) =>
        part.toLowerCase() === search.toLowerCase() ? (
          <mark key={i} className="bg-yellow-300 dark:bg-yellow-700 rounded px-0.5">{part}</mark>
        ) : (
          <span key={i}>{part}</span>
        )
      )}
    </>
  )
}

export function StructuredLogViewer({ taskId, taskPhase }: { taskId: string; taskPhase?: string }) {
  const { logs, isStreaming, isLive, error, refetch, clear } = useTaskLogs(taskId, true, taskPhase as import('@/schemas/task').TaskPhase | undefined)
  const [search, setSearch] = useState('')
  const [pinToBottom, setPinToBottom] = useState(true)
  const bottomRef = useRef<HTMLDivElement>(null)
  const scrollContainerRef = useRef<HTMLDivElement>(null)

  const filteredLogs = search
    ? logs.filter(line => line.toLowerCase().includes(search.toLowerCase()))
    : logs

  const scrollToBottom = useCallback(() => {
    if (bottomRef.current?.scrollIntoView) {
      bottomRef.current.scrollIntoView({ behavior: 'smooth' })
    }
  }, [])

  useEffect(() => {
    if (pinToBottom) scrollToBottom()
  }, [logs, pinToBottom, scrollToBottom])

  const handleScroll = useCallback(() => {
    const el = scrollContainerRef.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
    if (!atBottom && pinToBottom) setPinToBottom(false)
  }, [pinToBottom])

  if (!logs.length && !isStreaming && !error) {
    return (
      <Card>
        <CardHeader><CardTitle>Logs</CardTitle></CardHeader>
        <CardContent>
          {isStreaming ? (
            <div className="space-y-2">
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-3/4" />
              <Skeleton className="h-4 w-5/6" />
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">No logs available yet.</p>
          )}
        </CardContent>
      </Card>
    )
  }

  if (error) {
    return (
      <Card>
        <CardHeader><CardTitle>Logs</CardTitle></CardHeader>
        <CardContent>
          <div className="flex items-center gap-2">
            <p className="text-sm text-destructive">{error}</p>
            <Button variant="outline" size="sm" onClick={refetch}>
              <RefreshCw className="mr-1 h-3 w-3" /> Retry
            </Button>
          </div>
        </CardContent>
      </Card>
    )
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <CardTitle className="flex items-center gap-2">
            Logs
            <span className="text-xs font-normal text-muted-foreground">
              ({filteredLogs.length} line{filteredLogs.length !== 1 ? 's' : ''})
            </span>
            {isLive && (
              <span className="text-xs font-normal text-green-600 dark:text-green-400 animate-pulse">● Live</span>
            )}
            {isStreaming && !isLive && (
              <span className="text-xs font-normal text-blue-600 dark:text-blue-400">● Streaming</span>
            )}
          </CardTitle>
          <div className="flex items-center gap-2">
            <div className="relative">
              <Search className="absolute left-2 top-1/2 h-3 w-3 -translate-y-1/2 text-muted-foreground" />
              <input
                type="text"
                placeholder="Filter logs..."
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="h-8 w-48 rounded-md border bg-background pl-7 pr-7 text-sm"
                aria-label="Filter logs"
              />
              {search && (
                <button
                  onClick={() => setSearch('')}
                  className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                  aria-label="Clear filter"
                >
                  <X className="h-3 w-3" />
                </button>
              )}
            </div>
            <Button
              variant={pinToBottom ? 'default' : 'outline'}
              size="sm"
              onClick={() => {
                setPinToBottom(true)
                scrollToBottom()
              }}
              title="Pin to bottom"
            >
              <ArrowDown className="h-3 w-3" />
            </Button>
            <Button variant="outline" size="sm" onClick={clear}>Clear</Button>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        <ScrollArea className="h-[500px] w-full rounded-md border">
          <div
            ref={scrollContainerRef}
            onScroll={handleScroll}
            className="p-4 font-mono text-xs"
          >
            {filteredLogs.map((line, i) => {
              const level = parseLogLevel(line)
              return (
                <div key={i} className={`py-0.5 ${levelColors[level]}`} data-testid="log-line">
                  <span className="mr-3 select-none text-muted-foreground">{i + 1}</span>
                  <HighlightedText text={line} search={search} />
                </div>
              )
            })}
            <div ref={bottomRef} />
          </div>
        </ScrollArea>
      </CardContent>
    </Card>
  )
}
