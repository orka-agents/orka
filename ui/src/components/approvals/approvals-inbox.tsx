import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { toast } from 'sonner'
import { Inbox } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { isTerminalInboxError, useInboxDecision, usePendingApprovals } from '@/hooks/use-attention'
import { isNotImplementedError } from '@/lib/api-client'
import { approvalsPartialText, deadlineText, incompleteText } from '@/lib/attention-lanes'
import { timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import type { AttentionItem } from '@/schemas/attention'

const sourceLabel = { parked: 'Parked sign-off', brokered: 'Brokered tool', worker: 'Agent tool' } as const

function ApprovalRow({ item }: { item: AttentionItem }) {
  const decision = useInboxDecision()
  const [settled, setSettled] = useState<string | null>(null)
  const approval = item.approval
  const task = item.task
  if (!approval || !task) return null
  const due = deadlineText(item.deadline)
  const busy = decision.isPending

  const decline = () => {
    decision.mutate(
      { taskName: task.name, taskUID: task.uid, approvalId: approval.id, decision: 'decline' },
      {
        onSuccess: (result) => {
          if (result.outcome === 'alreadyDecided') {
            setSettled('Already decided')
            toast.info(`Approval on ${task.name} was already decided`)
          } else {
            setSettled('Declined')
            toast.success(`Declined the request on ${task.name}`)
          }
        },
        onError: (error) => toast.error(`Could not decline: ${error instanceof Error ? error.message : 'request failed'}`),
      },
    )
  }

  return (
    <li className="flex flex-col gap-3 py-4 sm:flex-row sm:items-center sm:justify-between">
      <div className="min-w-0 space-y-1">
        <p className="text-sm font-medium">{item.summary}</p>
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <Link to="/tasks/$taskId" params={{ taskId: task.name }} className="font-mono text-foreground underline-offset-2 hover:underline">
            {task.name}
          </Link>
          <Badge variant="outline">{sourceLabel[approval.source] ?? approval.source}</Badge>
          {approval.severity && <Badge variant="outline" className={cn(approval.severity === 'critical' && 'text-status-failed')}>{approval.severity}</Badge>}
          <span>requested {timeAgo(item.since)}</span>
          {due && <span className={cn('tabular-nums', due === 'overdue' && 'text-status-failed')}>{due}</span>}
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {settled ? (
          <span className="text-sm text-muted-foreground" role="status">{settled}</span>
        ) : (
          <>
            {approval.canApprove === true && (
              // Approval is never one click from the inbox: it opens the Task's
              // Approvals tab, which shows the exact operation and its inputs.
              <Button asChild size="sm">
                <Link to="/tasks/$taskId" params={{ taskId: task.name }} search={{ tab: 'approvals' }}>
                  Review to approve
                </Link>
              </Button>
            )}
            {approval.canDecline === true && (
              <Button size="sm" variant="outline" onClick={decline} disabled={busy}>
                {busy ? 'Declining…' : 'Decline'}
              </Button>
            )}
            {approval.canApprove !== true && approval.canDecline !== true && (
              <Link to="/tasks/$taskId" params={{ taskId: task.name }} search={{ tab: 'approvals' }} className="text-sm text-primary underline">
                Open Task
              </Link>
            )}
          </>
        )}
      </div>
    </li>
  )
}

/** Every decidable approval in the namespace, soonest deadline first. */
export function ApprovalsInbox() {
  const { data, isLoading, error, hasNextPage, fetchNextPage, isFetchingNextPage } = usePendingApprovals()
  // A 403 or 501 replaces the cached rows: polling has stopped, so keeping
  // them would leave stale decision controls up indefinitely. Any other
  // failure keeps the last loaded rows with a note that they are stale.
  const terminal = isTerminalInboxError(error)
  const shownError = error && (terminal || !data) ? error : null
  const staleError = error && !shownError ? error : null
  const pages = shownError ? [] : (data?.pages ?? [])
  const first = pages[0]
  // The server re-ranks on every read, so a page boundary can repeat an item.
  const items = [...new Map(pages.flatMap((page) => page.items).map((item) => [item.id, item])).values()]
  const incomplete = pages.some((page) => page.incomplete)
  const incompleteReasons = [...new Set(pages.flatMap((page) => page.incompleteReasons ?? []))]
  // Some Tasks that may be waiting for approval could not be read, so an
  // empty or short list is not "nothing is pending".
  const partial = pages.some((page) => page.sources?.approvals === 'partial')

  return (
    <div className="space-y-4">
      <PageHeader title="Approvals" description="Requests from agents waiting on a person, soonest deadline first" />
      {staleError && (
        <p className="text-sm text-status-failed" role="status">
          Couldn't refresh approvals ({staleError instanceof Error ? staleError.message : 'request failed'}). Showing the last loaded list.
        </p>
      )}
      {incomplete && (
        <p className="text-sm text-status-failed" role="status">{incompleteText(incompleteReasons)}</p>
      )}
      {partial && (
        <p className="text-sm text-status-failed" role="status">{approvalsPartialText} Their approvals are not listed.</p>
      )}
      {first && first.total > items.length && (
        <p className="text-sm text-muted-foreground" role="status">
          Showing {items.length.toLocaleString()} of {first.total.toLocaleString()} pending approvals.
        </p>
      )}
      <Card>
        <CardContent>
          {isLoading ? (
            <div className="space-y-3 py-2">
              {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-14 w-full" />)}
            </div>
          ) : shownError ? (
            isNotImplementedError(shownError) ? (
              <EmptyState icon={Inbox} headline="Approvals are unavailable" hint="Execution event storage is not enabled on this controller." />
            ) : (
              <ListAccessError error={shownError} resource="approvals" />
            )
          ) : items.length === 0 ? (
            <EmptyState
              icon={Inbox}
              headline={partial ? 'No pending approvals you can read' : 'No pending approvals'}
              hint={partial ? 'Requests on Tasks your token cannot read are not listed.' : 'Requests that need a decision appear here.'}
            />
          ) : (
            <>
              <ul className="divide-y" aria-label="Pending approvals">
                {items.map((item) => <ApprovalRow key={item.id} item={item} />)}
              </ul>
              {hasNextPage && (
                <div className="flex justify-center pt-3">
                  <Button variant="outline" size="sm" onClick={() => fetchNextPage()} disabled={isFetchingNextPage}>
                    {isFetchingNextPage ? 'Loading…' : 'Load more'}
                  </Button>
                </div>
              )}
            </>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
