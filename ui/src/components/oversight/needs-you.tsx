import { useState } from 'react'
import { CheckCircle2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { rowBadge, rowTooltip, type Incident } from '@/lib/oversight'
import { formatTimestamp, timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import { RowBadge } from './decision-table'
import { TONE_ROW } from './tones'

/** Cards shown before "Show more": an attention budget, not a feed. */
export const INCIDENT_BUDGET = 10

/** Incidents that need a person, one line each, most severe first. */
export function NeedsYouList({ incidents, onOpen }: { incidents: Incident[]; onOpen: (incident: Incident) => void }) {
  const [expanded, setExpanded] = useState(false)
  if (incidents.length === 0) {
    return (
      <EmptyState
        icon={CheckCircle2}
        headline="Nothing needs you"
        hint="No charter break, would-pause or would-hold in the loaded actions. Everything else is under All activity."
      />
    )
  }
  const shown = expanded ? incidents : incidents.slice(0, INCIDENT_BUDGET)
  return (
    <div className="space-y-2">
      <ul className="space-y-1.5" aria-label="Incidents that need you">
        {shown.map((incident) => {
          const newest = incident.decisions[0]
          return (
            <li key={incident.key}>
              <button
                type="button"
                onClick={() => onOpen(incident)}
                title={rowTooltip(newest)}
                data-incident={incident.kind}
                className={cn(
                  'flex w-full items-center gap-3 rounded-md border px-3 py-2 text-left hover:bg-accent focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none',
                  TONE_ROW[rowBadge(incident.badge).tone],
                )}
              >
                <RowBadge kind={incident.badge} />
                <span className="min-w-0 flex-1 truncate text-sm">{incident.title}</span>
                <span className="shrink-0 text-sm font-semibold tabular-nums" aria-label={`${incident.decisions.length} actions`}>
                  ×{incident.decisions.length}
                </span>
                <span className="shrink-0 text-xs text-muted-foreground tabular-nums" title={formatTimestamp(incident.lastSeen)}>
                  {timeAgo(incident.lastSeen)}
                </span>
              </button>
            </li>
          )
        })}
      </ul>
      {incidents.length > INCIDENT_BUDGET && (
        <div className="flex justify-center">
          <Button variant="ghost" size="sm" onClick={() => setExpanded((v) => !v)}>
            {expanded ? 'Show fewer' : `Show ${incidents.length - INCIDENT_BUDGET} more`}
          </Button>
        </div>
      )}
    </div>
  )
}
