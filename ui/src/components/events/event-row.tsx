import { useId, useState } from 'react'
import { ChevronRight, GitFork } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { SeverityIcon } from './severity-icon'
import {
  executionEventCategory,
  EXECUTION_EVENT_CATEGORY_LABELS,
  isTruncated,
} from '@/lib/execution-events'
import type { ExecutionEvent } from '@/schemas/execution-event'

function formatTime(ts?: string): string {
  if (!ts) return ''
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleTimeString(undefined, { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0')
}

// Pretty-print the redacted API content payload. The backend already sanitizes
// and truncates content before serving it, so this never exposes hidden raw data.
function stringifyContent(event: ExecutionEvent): string | null {
  if (event.content === undefined || event.content === null) return null
  try {
    return JSON.stringify(event.content, null, 2)
  } catch {
    return null
  }
}

function TruncationMarkers({ event }: { event: ExecutionEvent }) {
  const t = event.truncation
  if (!t || !isTruncated(event)) return null
  const parts: string[] = []
  if (t.summaryTruncated) parts.push(`summary truncated${t.summaryOriginalChars ? ` (${t.summaryOriginalChars} chars)` : ''}`)
  if (t.contentTextTruncated) parts.push(`text truncated${t.contentTextOriginalChars ? ` (${t.contentTextOriginalChars} chars)` : ''}`)
  if (t.contentJsonTruncated) parts.push(`payload truncated${t.contentJsonOriginalBytes ? ` (${t.contentJsonOriginalBytes} bytes)` : ''}`)
  return (
    <Badge variant="outline" className="border-status-pending/50 text-status-pending" title={parts.join('; ')}>
      truncated
    </Badge>
  )
}

export interface EventRowProps {
  event: ExecutionEvent
  // Show the originating task name (used by the session timeline).
  showTask?: boolean
  // Optional render slot for a task link (session timeline supplies this).
  taskLink?: (taskName: string) => React.ReactNode
  // Optional "fork from here" action.
  onFork?: (event: ExecutionEvent) => void
  // Optional copy handler for the redacted event payload.
  onCopy?: (event: ExecutionEvent) => void
}

export function EventRow({ event, showTask, taskLink, onFork, onCopy }: EventRowProps) {
  const [expanded, setExpanded] = useState(false)
  const payloadId = useId()
  const category = executionEventCategory(event.type)
  const contentJson = stringifyContent(event)
  const hasDisclosure = Boolean(contentJson || event.contentText)

  return (
    <div className="group/event rounded-md border bg-card px-3 py-2 text-sm" data-testid="event-row">
      <div className="flex items-start gap-2">
        <span className="mt-0.5">
          <SeverityIcon severity={event.severity} />
        </span>
        <span className="mt-0.5 w-12 shrink-0 select-none font-mono text-xs text-muted-foreground" title={`sequence ${event.seq}`}>
          #{event.seq}
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-medium">{event.type}</span>
            <Badge variant="secondary" className="text-[10px]">
              {EXECUTION_EVENT_CATEGORY_LABELS[category]}
            </Badge>
            {showTask && event.taskName && (
              <span className="text-xs text-muted-foreground">
                {taskLink ? taskLink(event.taskName) : event.taskName}
              </span>
            )}
            <TruncationMarkers event={event} />
            <span className="ml-auto font-mono text-xs text-muted-foreground" title={event.createdAt}>
              {formatTime(event.createdAt)}
            </span>
          </div>
          {event.summary && <p className="mt-1 break-words text-muted-foreground">{event.summary}</p>}
          <div className="mt-1 flex flex-wrap items-center gap-1.5">
            {event.toolName && (
              <Badge variant="outline" className="text-[10px]">tool: {event.toolName}</Badge>
            )}
            {event.agentName && (
              <Badge variant="outline" className="text-[10px]">agent: {event.agentName}</Badge>
            )}
            {event.toolCallID && (
              <Badge variant="outline" className="font-mono text-[10px]" title={event.toolCallID}>
                call: {event.toolCallID.length > 12 ? event.toolCallID.slice(0, 12) + '…' : event.toolCallID}
              </Badge>
            )}
          </div>
          {(hasDisclosure || onFork || onCopy) && (
            <div className="mt-1.5 flex items-center gap-3">
              {hasDisclosure && (
                <button
                  type="button"
                  onClick={() => setExpanded((v) => !v)}
                  className="inline-flex items-center gap-1 rounded text-xs text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  aria-expanded={expanded}
                  // Only advertise the relationship while the region is mounted, so
                  // assistive tech never follows a dangling IDREF when collapsed.
                  aria-controls={expanded ? payloadId : undefined}
                >
                  <ChevronRight className={`h-3 w-3 transition-transform ${expanded ? 'rotate-90' : ''}`} aria-hidden="true" />
                  {expanded ? 'Hide payload' : 'Show payload'}
                </button>
              )}
              {onCopy && (contentJson || event.contentText) && (
                <button
                  type="button"
                  onClick={() => onCopy(event)}
                  className="rounded text-xs text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                >
                  Copy JSON
                </button>
              )}
              {onFork && (
                <button
                  type="button"
                  onClick={() => onFork(event)}
                  className="inline-flex items-center gap-1 rounded text-xs text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                >
                  <GitFork className="h-3 w-3" aria-hidden="true" /> Fork from here
                </button>
              )}
            </div>
          )}
          {expanded && (
            <div className="mt-1.5 space-y-2" id={payloadId}>
              {event.contentText && (
                <pre className="overflow-x-auto rounded bg-muted p-2 text-xs whitespace-pre-wrap">{event.contentText}</pre>
              )}
              {contentJson && (
                <pre className="overflow-x-auto rounded bg-muted p-2 text-xs" data-testid="event-payload">{contentJson}</pre>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
