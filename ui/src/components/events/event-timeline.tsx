import { useMemo, useState } from 'react'
import { Radio, Search, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { EventRow } from './event-row'
import {
  executionEventCategory,
  normalizeSeverity,
  EXECUTION_EVENT_CATEGORY_LABELS,
  EXECUTION_EVENT_CATEGORY_ORDER,
  type ExecutionEventCategory,
  type ExecutionEventSeverityLevel,
} from '@/lib/execution-events'
import type { ExecutionEvent } from '@/schemas/execution-event'
import type { ExecutionStreamStatus } from '@/hooks/use-execution-event-stream'

const SEVERITY_FILTERS: { value: ExecutionEventSeverityLevel | 'all'; label: string }[] = [
  { value: 'all', label: 'All severities' },
  { value: 'info', label: 'Info' },
  { value: 'warning', label: 'Warning' },
  { value: 'error', label: 'Error' },
  { value: 'debug', label: 'Debug' },
]

export interface EventTimelineProps {
  events: ExecutionEvent[]
  // Live-follow status from the stream hook. Drives the live indicator.
  streamStatus?: ExecutionStreamStatus
  // Latest cursor (for the resume-from-seq helper text).
  lastSeq?: number
  isLoading?: boolean
  error?: string | null
  onRetry?: () => void
  // Follow toggle controls.
  following?: boolean
  onToggleFollow?: () => void
  // Per-row extras.
  showTask?: boolean
  taskLink?: (taskName: string) => React.ReactNode
  onFork?: (event: ExecutionEvent) => void
  emptyMessage?: string
  // Label for the surface (task vs session).
  title?: string
}

function copyEventJson(event: ExecutionEvent) {
  // Copy the redacted API payload exactly as served — never hidden raw data.
  const payload = JSON.stringify(event, null, 2)
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    void navigator.clipboard.writeText(payload)
  }
}

export function EventTimeline({
  events,
  streamStatus,
  lastSeq,
  isLoading,
  error,
  onRetry,
  following,
  onToggleFollow,
  showTask,
  taskLink,
  onFork,
  emptyMessage = 'No events yet.',
  title = 'Events',
}: EventTimelineProps) {
  const [search, setSearch] = useState('')
  const [severityFilter, setSeverityFilter] = useState<ExecutionEventSeverityLevel | 'all'>('all')
  const [categoryFilter, setCategoryFilter] = useState<ExecutionEventCategory | 'all'>('all')

  // Categories actually present, for a compact filter row.
  const presentCategories = useMemo(() => {
    const set = new Set<ExecutionEventCategory>()
    for (const e of events) set.add(executionEventCategory(e.type))
    return EXECUTION_EVENT_CATEGORY_ORDER.filter((c) => set.has(c))
  }, [events])

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    return events.filter((e) => {
      if (severityFilter !== 'all' && normalizeSeverity(e.severity) !== severityFilter) return false
      if (categoryFilter !== 'all' && executionEventCategory(e.type) !== categoryFilter) return false
      if (q) {
        const haystack = `${e.type} ${e.summary ?? ''} ${e.toolName ?? ''} ${e.agentName ?? ''} ${e.taskName ?? ''}`.toLowerCase()
        if (!haystack.includes(q)) return false
      }
      return true
    })
  }, [events, search, severityFilter, categoryFilter])

  const live = streamStatus === 'streaming' || streamStatus === 'connecting'
  const completed = streamStatus === 'complete'

  return (
    <div className="space-y-3" data-testid="event-timeline">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-semibold">{title}</h3>
        <span className="text-xs text-muted-foreground">
          {filtered.length} of {events.length} event{events.length !== 1 ? 's' : ''}
        </span>
        {live && (
          <span className="inline-flex items-center gap-1 text-xs font-medium text-green-600 dark:text-green-400">
            <Radio className="h-3 w-3 animate-pulse" aria-hidden="true" /> Live
          </span>
        )}
        {completed && (
          <Badge variant="secondary" className="text-[10px]">stream complete</Badge>
        )}
        {streamStatus === 'unsupported' && (
          <Badge variant="outline" className="text-[10px]">streaming unavailable</Badge>
        )}
        <div className="ml-auto flex items-center gap-2">
          {onToggleFollow && streamStatus !== 'unsupported' && (
            <Button variant={following ? 'default' : 'outline'} size="sm" onClick={onToggleFollow}>
              {following ? 'Stop following' : 'Follow live'}
            </Button>
          )}
        </div>
      </div>

      {events.length > 0 && (
        <div className="flex flex-wrap items-center gap-2">
          <div className="relative">
            <Search className="absolute left-2 top-1/2 h-3 w-3 -translate-y-1/2 text-muted-foreground" aria-hidden="true" />
            <input
              type="text"
              placeholder="Search summaries…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="h-8 w-48 rounded-md border bg-background pl-7 pr-7 text-sm"
              aria-label="Search events"
            />
            {search && (
              <button
                onClick={() => setSearch('')}
                className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                aria-label="Clear search"
              >
                <X className="h-3 w-3" />
              </button>
            )}
          </div>
          <select
            value={severityFilter}
            onChange={(e) => setSeverityFilter(e.target.value as ExecutionEventSeverityLevel | 'all')}
            className="h-8 rounded-md border bg-background px-2 text-sm"
            aria-label="Filter by severity"
          >
            {SEVERITY_FILTERS.map((s) => (
              <option key={s.value} value={s.value}>{s.label}</option>
            ))}
          </select>
          <select
            value={categoryFilter}
            onChange={(e) => setCategoryFilter(e.target.value as ExecutionEventCategory | 'all')}
            className="h-8 rounded-md border bg-background px-2 text-sm"
            aria-label="Filter by category"
          >
            <option value="all">All categories</option>
            {presentCategories.map((c) => (
              <option key={c} value={c}>{EXECUTION_EVENT_CATEGORY_LABELS[c]}</option>
            ))}
          </select>
        </div>
      )}

      {error && (
        <div className="flex items-center gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm">
          <span className="text-destructive">{error}</span>
          {onRetry && (
            <Button variant="outline" size="sm" onClick={onRetry}>Retry</Button>
          )}
        </div>
      )}

      {!error && events.length === 0 && (
        <div className="rounded-md border border-dashed px-4 py-8 text-center text-sm text-muted-foreground">
          {isLoading ? 'Loading events…' : emptyMessage}
        </div>
      )}

      {filtered.length > 0 && (
        <ul className="space-y-2" aria-label="Event timeline">
          {filtered.map((event) => (
            <li key={`${event.streamID}:${event.seq}`}>
              <EventRow
                event={event}
                showTask={showTask}
                taskLink={taskLink}
                onFork={onFork}
                onCopy={copyEventJson}
              />
            </li>
          ))}
        </ul>
      )}

      {events.length > 0 && filtered.length === 0 && (
        <div className="rounded-md border border-dashed px-4 py-6 text-center text-sm text-muted-foreground">
          No events match the current filters.
        </div>
      )}

      {typeof lastSeq === 'number' && lastSeq > 0 && (
        <p className="text-xs text-muted-foreground">
          Latest sequence <span className="font-mono">#{lastSeq}</span>. Resume from the API or CLI with{' '}
          <code className="rounded bg-muted px-1 py-0.5 font-mono">after={lastSeq}</code>.
        </p>
      )}
    </div>
  )
}
