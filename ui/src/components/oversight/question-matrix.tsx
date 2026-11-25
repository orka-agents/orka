import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatProbability, matrixCell, questionColumns } from '@/lib/oversight'
import { cn } from '@/lib/utils'
import type { DecisionView } from '@/schemas/oversight'
import { SignalBadge } from './signal'

/**
 * Recent actions against every concern or question their scores asked.
 * Cells stay muted unless the score was triggered or at its threshold.
 */
export function QuestionMatrix({ decisions, onOpen }: { decisions: DecisionView[]; onOpen: (id: string) => void }) {
  const columns = questionColumns(decisions)
  if (decisions.length === 0) return null
  if (columns.length === 0) {
    return <p className="py-6 text-center text-sm text-muted-foreground">No scorer asked any question about these actions.</p>
  }
  return (
    <Table scrollRegionLabel="Question by action matrix" aria-label="Question by action matrix">
      <TableHeader>
        <TableRow>
          <TableHead>Action</TableHead>
          <TableHead>Signal</TableHead>
          {columns.map((id) => (
            <TableHead key={id} className="text-center font-mono">{id}</TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {decisions.map((decision) => (
          <TableRow key={decision.id} className="cursor-pointer" onClick={() => onOpen(decision.id)}>
            <TableCell>
              <button
                type="button"
                className="text-left font-mono text-xs hover:underline focus-visible:underline focus-visible:outline-none"
                onClick={(event) => {
                  event.stopPropagation()
                  onOpen(decision.id)
                }}
              >
                {decision.action.toolName}
                <span className="block text-muted-foreground">{decision.taskName}</span>
              </button>
            </TableCell>
            <TableCell><SignalBadge signal={decision.signal} /></TableCell>
            {columns.map((id) => {
              const cell = matrixCell(decision, id)
              const empty = cell.probability === undefined && !cell.advisorNamed
              const text = cell.probability !== undefined ? formatProbability(cell.probability) : cell.advisorNamed ? 'named' : '·'
              const description = empty
                ? `${id}: not asked`
                : `${id}: ${text}${cell.threshold !== undefined ? ` against threshold ${formatProbability(cell.threshold)}` : ''}${cell.triggered ? ', triggered' : ''}`
              return (
                <TableCell
                  key={id}
                  title={description}
                  aria-label={description}
                  data-triggered={cell.triggered || undefined}
                  className={cn(
                    'text-center font-mono text-xs tabular-nums',
                    cell.triggered ? 'bg-status-pending-bg font-semibold text-status-pending' : 'text-muted-foreground/60',
                  )}
                >
                  {text}{cell.triggered && <span className="sr-only"> triggered</span>}
                </TableCell>
              )
            })}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
