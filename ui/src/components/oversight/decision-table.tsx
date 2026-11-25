import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { feedbackLabelText, isMonitorFailure, scoreVsThreshold, signalPresentation, topTriggeredScore } from '@/lib/oversight'
import { formatTimestamp, timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import type { DecisionView } from '@/schemas/oversight'
import { Chip, SignalBadge, WouldPauseChip } from './signal'
import { TONE_ROW } from './tones'

/** The one-line reason a row reads the way it does. */
export function DecisionFinding({ decision }: { decision: DecisionView }) {
  switch (decision.signal) {
    case 'against_charter': {
      const finding = decision.charterFindings?.[0]
      if (finding) {
        return (
          <span className="text-status-failed">
            Against the charter: <q className="italic">{finding.sentence}</q>
          </span>
        )
      }
      return <span className="text-status-failed">Held for a person</span>
    }
    case 'monitor_unavailable': {
      const failed = [...(decision.scorers ?? []), ...(decision.advisors ?? [])].find((p) => isMonitorFailure(p.status))
      return (
        <span className="text-status-failed">
          Scorer unavailable → would hold (never a silent pass)
          {failed && <span className="text-muted-foreground"> · {failed.status}</span>}
        </span>
      )
    }
    case 'risky': {
      const top = topTriggeredScore(decision)
      if (top) {
        return (
          <span className="text-status-pending">
            Looks risky: <span className="font-mono tabular-nums">{scoreVsThreshold(top)}</span>
            {top.label && <span className="text-muted-foreground"> {top.label}</span>}
          </span>
        )
      }
      const concern = decision.advisors?.flatMap((a) => a.concerns ?? [])[0]
      return <span className="text-status-pending">Looks risky{concern ? `: advisor named ${concern}` : ''}</span>
    }
    default:
      return <span>{signalPresentation(decision.signal).label}</span>
  }
}

export interface DecisionTableProps {
  decisions: DecisionView[]
  onOpen: (id: string) => void
  /** Selection is offered only when these are set. */
  selectedIds?: Set<string>
  onToggleSelect?: (id: string) => void
  onToggleAll?: (select: boolean) => void
  label?: string
  /** Hide the Task column, as on a Task's own tab. */
  hideTask?: boolean
}

export function DecisionTable({ decisions, onOpen, selectedIds, onToggleSelect, onToggleAll, label = 'Oversight decisions', hideTask }: DecisionTableProps) {
  const selectable = Boolean(selectedIds && onToggleSelect)
  const allSelected = selectable && decisions.length > 0 && decisions.every((d) => selectedIds!.has(d.id))
  return (
    <Table scrollRegionLabel={label} aria-label={label}>
      <TableHeader>
        <TableRow>
          {selectable && (
            <TableHead className="w-8">
              <input
                type="checkbox"
                aria-label="Select all shown"
                checked={allSelected}
                onChange={(event) => onToggleAll?.(event.target.checked)}
              />
            </TableHead>
          )}
          <TableHead>Signal</TableHead>
          <TableHead>Action</TableHead>
          {!hideTask && <TableHead>Task</TableHead>}
          <TableHead>Why</TableHead>
          <TableHead className="text-right">When</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {decisions.map((decision) => {
          const tone = signalPresentation(decision.signal).tone
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
              <TableCell>
                <div className="flex flex-col gap-1">
                  <SignalBadge signal={decision.signal} />
                  {decision.wouldHavePaused && <WouldPauseChip />}
                </div>
              </TableCell>
              <TableCell className="max-w-md">
                <button
                  type="button"
                  className="text-left font-mono text-sm font-medium hover:underline focus-visible:underline focus-visible:outline-none"
                  onClick={(event) => {
                    event.stopPropagation()
                    onOpen(decision.id)
                  }}
                  aria-label={`Inspect ${decision.action.toolName} action ${decision.id}`}
                >
                  {decision.action.toolName}
                </button>
                {decision.actionNarrative && (
                  <p className={cn('line-clamp-2 text-xs whitespace-normal', tone === 'muted' ? 'text-muted-foreground/80' : 'text-muted-foreground')}>
                    {decision.actionNarrative}
                  </p>
                )}
              </TableCell>
              {!hideTask && (
                <TableCell className="text-xs">
                  <div className="font-mono">{decision.taskName}</div>
                  {decision.agentName && <div className="text-muted-foreground">{decision.agentName}</div>}
                </TableCell>
              )}
              <TableCell className="max-w-sm text-xs whitespace-normal">
                <DecisionFinding decision={decision} />
                {decision.feedback && (
                  <div className="mt-1"><Chip>Labelled: {feedbackLabelText(decision.feedback.label)}</Chip></div>
                )}
              </TableCell>
              <TableCell className="text-right text-xs text-muted-foreground tabular-nums" title={formatTimestamp(decision.observedAt)}>
                {timeAgo(decision.observedAt)}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
