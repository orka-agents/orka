import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'
import {
  executionEventCategory,
  normalizeSeverity,
  EXECUTION_EVENT_CATEGORY_LABELS,
  type ExecutionEventCategory,
} from '@/lib/execution-events'
import type { ExecutionEvent } from '@/schemas/execution-event'

// Filter buckets: 'all' shows everything, category buckets reuse the shared
// taxonomy, and 'errors' is severity-driven (error + warning) since failures
// stay grouped with their functional area rather than a category of their own.
type TimelineFilter = 'all' | ExecutionEventCategory | 'errors'

const FILTERS: { value: TimelineFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'lifecycle', label: EXECUTION_EVENT_CATEGORY_LABELS.lifecycle },
  { value: 'model', label: EXECUTION_EVENT_CATEGORY_LABELS.model },
  { value: 'tools', label: EXECUTION_EVENT_CATEGORY_LABELS.tools },
  { value: 'approvals', label: EXECUTION_EVENT_CATEGORY_LABELS.approvals },
  { value: 'artifacts', label: EXECUTION_EVENT_CATEGORY_LABELS.artifacts },
  { value: 'errors', label: 'Errors / Warnings' },
]

function matchesFilter(event: ExecutionEvent, filter: TimelineFilter): boolean {
  if (filter === 'all') return true
  if (filter === 'errors') {
    const sev = normalizeSeverity(event.severity)
    return sev === 'error' || sev === 'warning'
  }
  return executionEventCategory(event.type) === filter
}

function severityClass(severity: string): string {
  switch (normalizeSeverity(severity)) {
    case 'error':
      return 'text-status-failed'
    case 'warning':
      return 'text-status-pending'
    default:
      return 'text-muted-foreground'
  }
}

/**
 * Read-only timeline of a task's execution events. Errors and warnings are never
 * hidden by default — the 'all' filter keeps them inline so failures stay visible
 * until the operator explicitly narrows to a non-error category. Rows preserve
 * the incoming seq order; `events` is assumed sorted ascending.
 */
export function RuntimeTimeline({
  events,
  status,
}: {
  events: ExecutionEvent[]
  status?: string
}) {
  const [filter, setFilter] = useState<TimelineFilter>('all')
  const visible = events.filter((e) => matchesFilter(e, filter))

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap gap-1.5" role="group" aria-label="Event filters">
        {FILTERS.map((f) => (
          <Button
            key={f.value}
            type="button"
            size="xs"
            variant={filter === f.value ? 'secondary' : 'ghost'}
            aria-pressed={filter === f.value}
            onClick={() => setFilter(f.value)}
          >
            {f.label}
          </Button>
        ))}
      </div>

      {status === 'unsupported' ? (
        <p className="text-sm text-muted-foreground">Live stream not enabled</p>
      ) : (
        <>
          {status === 'error' && (
            <p className="text-sm text-destructive" role="alert">Unable to load events</p>
          )}
          {visible.length > 0 ? (
            <ul className="space-y-px">
              {visible.map((e) => (
                <li
                  key={e.id}
                  className="flex items-baseline gap-2 px-1 py-0.5 text-xs"
                >
                  <span className="w-10 shrink-0 text-right tabular-nums text-muted-foreground">
                    {e.seq}
                  </span>
                  <span className={cn('w-44 shrink-0 truncate font-mono', severityClass(e.severity))}>
                    {e.type}
                  </span>
                  <span className="truncate text-muted-foreground">{e.summary ?? ''}</span>
                </li>
              ))}
            </ul>
          ) : status !== 'error' ? (
            <p className="text-sm text-muted-foreground">No events</p>
          ) : null}
        </>
      )}

      {status === 'complete' && (
        <p className="text-xs text-muted-foreground" role="status">
          — stream complete —
        </p>
      )}
    </div>
  )
}
