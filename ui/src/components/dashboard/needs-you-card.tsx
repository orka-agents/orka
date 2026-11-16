import { Link } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { AttentionItemCard } from '@/components/attention/attention-item-card'
import { useAttention } from '@/hooks/use-attention'
import { isForbiddenError } from '@/lib/api-client'
import { approvalsUnreadText, attentionKindLabels, attentionSourceNotes, incompleteText } from '@/lib/attention-lanes'
import type { AttentionKind } from '@/schemas/attention'

const kinds: AttentionKind[] = ['approval', 'outcomeUnknown', 'failed', 'podWaiting', 'monitorBlocked']

/** The dashboard's top attention items, linking to the full board. */
export function NeedsYouCard({ limit = 5 }: { limit?: number }) {
  const { data: cached, isLoading, error } = useAttention(limit, 30000)
  // A 403 replaces the cached items: polling has stopped, so they would stay
  // up as current after access was revoked. Any other failure keeps the last
  // loaded items with a note that they are stale.
  const shownError = error && (isForbiddenError(error) || !cached) ? error : null
  const staleError = error && !shownError ? error : null
  const data = shownError ? undefined : cached
  const items = data?.items ?? []
  const notes = data ? attentionSourceNotes(data.sources) : undefined

  return (
    <Card aria-labelledby="needs-you-title">
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <CardTitle id="needs-you-title" className="text-sm font-medium">
          Needs you
          {data && (
            <span className="ml-2 inline-flex items-center justify-center rounded-full border px-2 py-0.5 text-xs tabular-nums">
              {data.total.toLocaleString()}{data.incomplete ? '+' : ''}
            </span>
          )}
        </CardTitle>
        <div className="flex items-center gap-3 text-sm">
          <Link to="/approvals" className="text-primary underline">Approvals</Link>
          <Link to="/kanban" search={{ view: 'attention' }} className="text-primary underline">Board</Link>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : shownError ? (
          <p className="text-sm text-muted-foreground" role="alert">
            {isForbiddenError(shownError) ? 'Your token cannot list tasks in this namespace.' : `Could not load attention items (${shownError instanceof Error ? shownError.message : "request failed"}).`}
          </p>
        ) : items.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            {notes?.approvalsUnread ? 'Nothing else needs you right now, but approvals were not checked.' : 'Nothing needs you right now.'}
          </p>
        ) : (
          <>
            <p className="text-xs text-muted-foreground">
              {kinds
                .filter((kind) => (data?.counts[kind] ?? 0) > 0)
                .map((kind) => `${data?.counts[kind]} ${attentionKindLabels[kind].toLowerCase()}`)
                .join(' · ')}
            </p>
            <div className="grid gap-2 md:grid-cols-2 xl:grid-cols-3">
              {items.map((item) => <AttentionItemCard key={item.id} item={item} />)}
            </div>
          </>
        )}
        {staleError && (
          <p className="text-xs text-status-failed" role="status">
            Couldn't refresh attention items ({staleError instanceof Error ? staleError.message : 'request failed'}). Showing the last loaded list.
          </p>
        )}
        {data?.incomplete && (
          <p className="text-xs text-status-failed" role="status">{incompleteText(data.incompleteReasons)}</p>
        )}
        {notes?.approvalsUnread && (
          <p className="text-xs text-status-failed" role="status">{approvalsUnreadText} Tasks waiting for approval are not listed.</p>
        )}
        {notes?.monitorsNote && <p className="text-xs text-muted-foreground">{notes.monitorsNote}</p>}
      </CardContent>
    </Card>
  )
}
