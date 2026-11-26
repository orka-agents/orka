import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useOversightConfig } from '@/hooks/use-oversight'
import { feedbackLabelText, rowBadge, rowBadgeKind, rowSummary, rowTooltip, type RowBadgeKind } from '@/lib/oversight'
import { formatTimestamp, timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import type { DecisionView } from '@/schemas/oversight'
import { TONE_BADGE, TONE_ROW } from './tones'
import { PendingRow } from './pending-row'
import type { PendingToolCall } from '@/hooks/use-pending-tool-calls'

/** The single most severe badge a row or incident carries, always in words. */
export function RowBadge({ kind, className }: { kind: RowBadgeKind; className?: string }) {
  const badge = rowBadge(kind)
  return (
    <span
      data-badge={kind}
      className={cn('inline-flex w-fit shrink-0 items-center whitespace-nowrap rounded-full border px-2 py-0.5 text-xs font-medium', TONE_BADGE[badge.tone], className)}
    >
      {badge.label}
    </span>
  )
}

export interface DecisionTableProps {
  decisions: DecisionView[]
  onOpen: (id: string) => void
  /** Selection is offered only when these are set. */
  selectedIds?: Set<string>
  onToggleSelect?: (id: string) => void
  onToggleAll?: (select: boolean) => void
  label?: string
  /** Provisional "seen, scoring…" rows, shown first; never selectable or clickable. */
  pending?: PendingToolCall[]
}

/**
 * One line per action: the most severe badge, a plain-language summary built
 * from its fields, and when. Raw tool text and IDs are on hover and in the
 * drawer.
 */
export function DecisionTable({ decisions, onOpen, selectedIds, onToggleSelect, onToggleAll, label = 'Oversight decisions', pending = [] }: DecisionTableProps) {
  const { data: config } = useOversightConfig()
  const selectable = Boolean(selectedIds && onToggleSelect)
  const allSelected = selectable && decisions.length > 0 && decisions.every((d) => selectedIds!.has(d.id))
  return (
    <Table scrollRegionLabel={label} aria-label={label}>
      <TableHeader>
        <TableRow>
          {selectable && (
            <TableHead className="w-8">
              <input type="checkbox" aria-label="Select all shown" checked={allSelected} onChange={(event) => onToggleAll?.(event.target.checked)} />
            </TableHead>
          )}
          <TableHead className="w-0">Signal</TableHead>
          <TableHead>What happened</TableHead>
          <TableHead className="text-right">When</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {pending.map((call) => <PendingRow key={call.key} call={call} columns={selectable ? 4 : 3} />)}
        {decisions.map((decision) => {
          const kind = rowBadgeKind(decision)
          const tone = rowBadge(kind).tone
          return (
            <TableRow
              key={decision.id}
              data-signal={decision.signal}
              className={cn('cursor-pointer', TONE_ROW[tone])}
              onClick={() => onOpen(decision.id)}
            >
              {selectable && (
                <TableCell onClick={(event) => event.stopPropagation()}>
                  <input
                    type="checkbox"
                    aria-label={`Select ${decision.id}`}
                    checked={selectedIds!.has(decision.id)}
                    onChange={() => onToggleSelect!(decision.id)}
                  />
                </TableCell>
              )}
              <TableCell><RowBadge kind={kind} /></TableCell>
              <TableCell className="max-w-xl whitespace-normal">
                <button
                  type="button"
                  title={rowTooltip(decision)}
                  className={cn('text-left text-sm hover:underline focus-visible:underline focus-visible:outline-none', tone === 'muted' && 'text-muted-foreground')}
                  onClick={(event) => {
                    event.stopPropagation()
                    onOpen(decision.id)
                  }}
                >
                  {rowSummary(decision, config)}
                </button>
                {decision.feedback && (
                  <span className="ml-2 text-xs text-muted-foreground">· labelled {feedbackLabelText(decision.feedback.label).toLowerCase()}</span>
                )}
              </TableCell>
              <TableCell className="text-right text-xs whitespace-nowrap text-muted-foreground tabular-nums" title={formatTimestamp(decision.observedAt)}>
                {timeAgo(decision.observedAt)}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
