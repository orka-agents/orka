import { Link } from '@tanstack/react-router'
import { CircleHelp, Hand, OctagonX, PackageX, Radar } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import { timeAgo } from '@/lib/time'
import { attentionKindLabels, deadlineText } from '@/lib/attention-lanes'
import type { AttentionItem, AttentionKind } from '@/schemas/attention'

const kindStyle: Record<AttentionKind, { icon: typeof Hand; rail: string; text: string }> = {
  approval: { icon: Hand, rail: 'border-status-running', text: 'text-status-running' },
  outcomeUnknown: { icon: CircleHelp, rail: 'border-status-finalizing', text: 'text-status-finalizing' },
  failed: { icon: OctagonX, rail: 'border-status-failed', text: 'text-status-failed' },
  podWaiting: { icon: PackageX, rail: 'border-status-pending', text: 'text-status-pending' },
  monitorBlocked: { icon: Radar, rail: 'border-border', text: 'text-muted-foreground' },
}

const sourceLabel = { parked: 'Parked sign-off', brokered: 'Brokered tool', worker: 'Agent tool' } as const

/**
 * One "Needs you" item. It shows only the server's safe summary and links to
 * the authorized detail page, where the operation can be inspected.
 */
export function AttentionItemCard({ item, className }: { item: AttentionItem; className?: string }) {
  const style = kindStyle[item.kind] ?? kindStyle.monitorBlocked
  const Icon = style.icon
  const label = attentionKindLabels[item.kind] ?? item.kind
  const due = deadlineText(item.deadline)
  const body = (
    <Card className={cn('gap-0 py-3 border-l-4 transition-all hover:shadow-md motion-safe:hover:-translate-y-0.5', style.rail, className)}>
      <CardContent className="px-3 py-0 space-y-1.5">
        <div className="flex items-center justify-between gap-2">
          <span className={cn('inline-flex items-center gap-1.5 text-xs font-medium', style.text)}>
            <Icon className="size-3.5" aria-hidden="true" />
            {label}
          </span>
          {due && (
            <span className={cn('text-xs tabular-nums', due === 'overdue' ? 'text-status-failed' : 'text-muted-foreground')}>{due}</span>
          )}
        </div>
        <p className="text-sm font-medium leading-snug">{item.summary}</p>
        <div className="flex items-center gap-2 text-xs text-muted-foreground flex-wrap">
          {item.task && <span className="font-mono truncate" title={item.task.name}>{item.task.name}</span>}
          {item.monitor && <span className="font-mono truncate" title={item.monitor.name}>{item.monitor.name}</span>}
          {item.approval && <Badge variant="outline">{sourceLabel[item.approval.source] ?? item.approval.source}</Badge>}
          <span>{timeAgo(item.since)}</span>
        </div>
      </CardContent>
    </Card>
  )
  if (item.task) {
    return (
      <Link
        to="/tasks/$taskId"
        params={{ taskId: item.task.name }}
        search={{ tab: item.kind === 'approval' ? 'approvals' : undefined }}
        className="block"
        aria-label={`${label}: ${item.summary} (${item.task.name})`}
      >
        {body}
      </Link>
    )
  }
  if (item.monitor) {
    return (
      <Link to="/monitors/$monitorId" params={{ monitorId: item.monitor.name }} className="block" aria-label={`${label}: ${item.summary}`}>
        {body}
      </Link>
    )
  }
  return body
}
