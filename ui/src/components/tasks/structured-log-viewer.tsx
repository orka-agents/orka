import { useRef, useEffect, useState, useCallback } from 'react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { useTaskLogs } from '@/hooks/use-task-logs'
import { ArrowDown, Search, X, RefreshCw, ScrollText } from 'lucide-react'
import { EmptyState } from '@/components/ui/empty-state'

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

function sameLines(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((line, i) => line === b[i])
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
  const { logs, isStreaming, isLive, error, unavailable, refetch, clear } = useTaskLogs(taskId, true, taskPhase as import('@/schemas/task').TaskPhase | undefined)
  const [search, setSearch] = useState('')
  const [pinToBottom, setPinToBottom] = useState(true)
  const scrollContainerRef = useRef<HTMLDivElement>(null)
  // While the reader is scrolled up, hold the lines they are reading: a
  // running task refetches a sliding tail window every poll, which would
  // shift rows (and restart their numbers) under them. Pinning resumes.
  const [frozen, setFrozen] = useState<{ taskId: string; lines: string[] } | null>(null)
  const frozenLines = !pinToBottom && frozen?.taskId === taskId ? frozen.lines : null
  const shownLogs = frozenLines ?? logs
  const newLinesWaiting = frozenLines !== null && !sameLines(frozenLines, logs)

  const indexedLogs = shownLogs.map((line, index) => ({ index: index + 1, line }))
  // Sized for the widest row number so text does not shift at 10, 100, ...
  const lineNumberWidth = `${String(shownLogs.length).length}ch`

  const filteredLogs = search
    ? indexedLogs.filter(({ line }) => line.toLowerCase().includes(search.toLowerCase()))
    : indexedLogs

  // Scroll the log container itself: scrollIntoView on a bottom marker also
  // scrolls every ancestor, which moved the whole page when the tab opened.
  const scrollToBottom = useCallback(() => {
    const el = scrollContainerRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [])

  useEffect(() => {
    if (pinToBottom) scrollToBottom()
  }, [logs, pinToBottom, scrollToBottom])

  const handleScroll = useCallback(() => {
    const el = scrollContainerRef.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
    if (!atBottom && pinToBottom) {
      setPinToBottom(false)
      setFrozen({ taskId, lines: logs })
    }
  }, [pinToBottom, taskId, logs])

  const jumpToLatest = useCallback(() => {
    setPinToBottom(true)
    scrollToBottom()
  }, [scrollToBottom])

  // No Job or pod yet (pending, scheduled parents, runtime-pool agents) is
  // expected, not a failure: explain it with the server's reason instead of
  // an error and Retry. Running tasks keep polling, so logs replace this.
  if (!shownLogs.length && unavailable && !error) {
    return (
      <Card>
        <CardHeader><CardTitle>Logs</CardTitle></CardHeader>
        <CardContent>
          <EmptyState
            headline="No pod logs for this task"
            hint={`${unavailable.charAt(0).toUpperCase()}${unavailable.slice(1)}. Logs appear once the task's pod starts; tasks that only schedule child tasks or run on an agent runtime pool may have none. The Runtime and Timeline tabs show their activity.`}
            icon={ScrollText}
            action={
              <Button variant="outline" size="sm" onClick={refetch} disabled={isStreaming}>
                <RefreshCw className="mr-1 h-3 w-3" /> Check again
              </Button>
            }
          />
        </CardContent>
      </Card>
    )
  }

  if (!shownLogs.length && !isStreaming && !error) {
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
            <EmptyState headline="No logs available yet." icon={ScrollText} />
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
        {/* The controls wrap below the title on narrow cards, and the filter
            grows to the row instead of pushing Pin and Clear off the card. */}
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle className="flex items-center gap-2">
            Logs
            <span className="text-xs font-normal text-muted-foreground">
              {search
                ? `(${filteredLogs.length} of ${shownLogs.length} loaded line${shownLogs.length !== 1 ? 's' : ''})`
                : `(${filteredLogs.length} line${filteredLogs.length !== 1 ? 's' : ''})`}
            </span>
            {isLive && (
              <span className="flex items-center gap-1 text-xs font-normal text-live">
                <span className="inline-block size-1.5 rounded-full bg-live motion-safe:animate-pulse-live" aria-hidden="true" />
                Live
              </span>
            )}
            {isStreaming && !isLive && (
              <span className="flex items-center gap-1 text-xs font-normal text-live">
                <span className="inline-block size-1.5 rounded-full bg-live" aria-hidden="true" />
                Streaming
              </span>
            )}
          </CardTitle>
          <div className="flex w-full items-center gap-2 sm:w-auto">
            <div className="relative min-w-0 flex-1 sm:flex-none">
              <Search className="absolute left-2 top-1/2 h-3 w-3 -translate-y-1/2 text-muted-foreground" />
              <input
                type="text"
                placeholder="Filter logs..."
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="h-8 w-full rounded-md border bg-background pl-7 pr-7 text-sm sm:w-48"
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
              onClick={jumpToLatest}
              title="Pin to bottom"
              aria-label="Pin logs to bottom"
            >
              <ArrowDown className="h-3 w-3" />
            </Button>
            {/* Clearing starts over from the newest output, so it follows it. */}
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                clear()
                setPinToBottom(true)
              }}
              aria-label="Clear logs"
            >
              Clear
            </Button>
          </div>
        </div>
      </CardHeader>
      <CardContent className="relative">
        {/* A native scroller, so the ref and onScroll sit on the element
            that actually scrolls; it takes focus so the keyboard can
            scroll it too. */}
        <div
          ref={scrollContainerRef}
          onScroll={handleScroll}
          tabIndex={0}
          role="region"
          aria-label="Log output"
          className="h-[500px] w-full overflow-auto rounded-md border p-4 font-mono text-xs"
        >
          {search && filteredLogs.length === 0 && (
            <div className="py-4 text-sm text-muted-foreground" data-testid="log-no-matches">
              {`No matching lines for "${search}".`}
            </div>
          )}
          {filteredLogs.map(row => {
            const level = parseLogLevel(row.line)
            return (
              <div key={`log-${row.index}`} className={`py-0.5 ${levelColors[level]}`} data-testid="log-line">
                <span className="mr-3 inline-block select-none text-right tabular-nums text-muted-foreground" style={{ width: lineNumberWidth }}>{row.index}</span>
                <HighlightedText text={row.line} search={search} />
              </div>
            )
          })}
        </div>
        {newLinesWaiting && (
          <Button
            variant="secondary"
            size="sm"
            onClick={jumpToLatest}
            className="absolute bottom-3 left-1/2 -translate-x-1/2 shadow-md"
          >
            <ArrowDown className="mr-1 h-3 w-3" /> New lines · Jump to latest
          </Button>
        )}
      </CardContent>
    </Card>
  )
}
