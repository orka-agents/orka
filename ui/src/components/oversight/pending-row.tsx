import { useEffect, useState } from 'react'
import { Loader2 } from 'lucide-react'
import { TableCell, TableRow } from '@/components/ui/table'
import { humanizeDuration } from '@/lib/time'
import type { PendingToolCall } from '@/hooks/use-pending-tool-calls'

const PENDING_TITLE = "Orka saw this tool call; the monitor's decision has not arrived yet"

function useNow(intervalMs: number) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(timer)
  }, [intervalMs])
  return now
}

/**
 * A provisional row: a completed tool call with no decision yet. It has no
 * signal badge, is not clickable, and its age counts up until the decision
 * replaces it.
 */
export function PendingRow({ call, columns }: { call: PendingToolCall; columns: number }) {
  const now = useNow(1000)
  const created = Date.parse(call.createdAt)
  const age = Number.isFinite(created) ? humanizeDuration((now - created) / 1000) : '?'
  return (
    <TableRow data-pending="true" title={PENDING_TITLE} className="border-dashed text-muted-foreground hover:bg-transparent">
      <TableCell colSpan={columns - 2} className="border-y border-dashed">
        <span className="inline-flex items-center gap-1.5 rounded-full border border-dashed border-border px-2 py-0.5 text-xs">
          <Loader2 className="size-3 animate-spin motion-reduce:animate-none" aria-hidden="true" />
          Seen · scoring…
        </span>
      </TableCell>
      <TableCell className="border-y border-dashed text-sm whitespace-normal">
        {call.agentName || call.taskName} called {call.toolName} · scoring…
      </TableCell>
      <TableCell className="border-y border-dashed text-right text-xs whitespace-nowrap tabular-nums" aria-label={`seen ${age} ago`}>
        {age}
      </TableCell>
    </TableRow>
  )
}

/** "3 tool calls seen, awaiting decisions · 1 with no decision after 10 min · watching 30 of 42 running Tasks". */
export function PendingNote({ pending, expired, watching, candidates }: { pending: number; expired: number; watching: number; candidates: number }) {
  const parts = [
    pending > 0 ? `${pending.toLocaleString()} tool ${pending === 1 ? 'call' : 'calls'} seen, awaiting decisions` : '',
    expired > 0 ? `${expired.toLocaleString()} with no decision after 10 min` : '',
    candidates > watching ? `watching ${watching} of ${candidates.toLocaleString()} running Tasks` : '',
  ].filter(Boolean)
  if (parts.length === 0) return null
  return <p className="text-xs text-muted-foreground" data-testid="pending-note">{parts.join(' · ')}</p>
}
