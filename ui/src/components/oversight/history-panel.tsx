import { useState } from 'react'
import { History } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ListAccessError } from '@/components/ui/list-access-error'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useOversightGroups } from '@/hooks/use-oversight'
import { queryTerm } from '@/lib/oversight'
import { formatTimestamp } from '@/lib/time'
import { cn } from '@/lib/utils'
import { PivotButton } from './signal'

const dateOnly = (ts: string) => {
  const parsed = new Date(ts)
  return Number.isNaN(parsed.getTime()) ? ts : parsed.toLocaleDateString()
}

/** "Has this happened before?": a search's matches grouped by agent or Task. */
export function HistoryPanel({ q, window, onPivot }: { q: string; window?: string; onPivot: (q: string) => void }) {
  const [by, setBy] = useState<'agent' | 'task'>('agent')
  const { data, error, isLoading } = useOversightGroups(q, by, window, true)
  const groups = data?.groups ?? []
  return (
    <Card role="region" aria-label="Has this happened before?">
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle className="flex items-center gap-2 text-base">
          <History className="size-4" aria-hidden="true" /> Has this happened before?
        </CardTitle>
        <div className="flex items-center gap-1" role="group" aria-label="Group by">
          {(['agent', 'task'] as const).map((value) => (
            <Button key={value} size="xs" variant={by === value ? 'secondary' : 'ghost'} aria-pressed={by === value} onClick={() => setBy(value)}>
              By {value}
            </Button>
          ))}
        </div>
      </CardHeader>
      <CardContent className="space-y-2">
        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : error ? (
          <ListAccessError error={error} resource="decision groups" />
        ) : (
          <>
            <p className="text-xs text-muted-foreground" role="status">
              Searched {(data?.searched ?? 0).toLocaleString()} actions{window && window !== 'all' ? ` in the last ${window}` : ' across all time'}
              {data?.truncated ? '; more matched than were read, so counts are a lower bound' : ''}.
            </p>
            {groups.length === 0 ? (
              <p className="text-sm text-muted-foreground">Nothing matched: this has not been seen before.</p>
            ) : (
              <Table scrollRegionLabel="Groups">
                <TableHeader>
                  <TableRow>
                    <TableHead>{by === 'agent' ? 'Agent' : 'Task'}</TableHead>
                    <TableHead className="text-right">Actions</TableHead>
                    <TableHead className="text-right">Risky</TableHead>
                    <TableHead className="text-right">Against charter</TableHead>
                    <TableHead>First seen</TableHead>
                    <TableHead>Last seen</TableHead>
                    {by === 'agent' && <TableHead>Tasks</TableHead>}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {groups.map((group) => (
                    <TableRow key={group.key || '(none)'}>
                      <TableCell>
                        {group.key ? (
                          <PivotButton query={queryTerm(by, group.key)} onPivot={onPivot}>{group.key}</PivotButton>
                        ) : (
                          <span className="text-xs text-muted-foreground">(no {by})</span>
                        )}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{group.count}</TableCell>
                      <TableCell className={cn('text-right tabular-nums', group.risky > 0 && 'font-medium text-status-pending')}>{group.risky}</TableCell>
                      <TableCell className={cn('text-right tabular-nums', group.againstCharter > 0 && 'font-medium text-status-failed')}>{group.againstCharter}</TableCell>
                      <TableCell className="text-xs" title={formatTimestamp(group.firstSeen)}>{dateOnly(group.firstSeen)}</TableCell>
                      <TableCell className="text-xs" title={formatTimestamp(group.lastSeen)}>{dateOnly(group.lastSeen)}</TableCell>
                      {by === 'agent' && (
                        <TableCell className="text-xs font-mono">{(group.tasks ?? []).join(', ')}</TableCell>
                      )}
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
