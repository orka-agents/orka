import { useState } from 'react'
import { Inbox } from 'lucide-react'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import {
  isMemoryStoreUnavailable,
  useApplyMemoryProposal,
  useArchiveMemoryProposal,
  useMemoryProposalList,
  useReviewMemoryProposal,
} from '@/hooks/use-memory'
import type { MemoryProposal } from '@/schemas/memory'
import { cn } from '@/lib/utils'

const STATUS_FILTERS = ['pending', 'accepted', 'rejected', 'applied', 'archived', 'all'] as const

function statusBadgeClass(status: string): string {
  switch (status) {
    case 'pending':
      return 'bg-status-pending-bg text-status-pending'
    case 'accepted':
      return 'bg-status-running-bg text-status-running'
    case 'applied':
      return 'bg-status-succeeded-bg text-status-succeeded'
    case 'rejected':
      return 'bg-status-failed-bg text-status-failed'
    default:
      return ''
  }
}

function ProposalCard({ proposal }: { proposal: MemoryProposal }) {
  const review = useReviewMemoryProposal()
  const apply = useApplyMemoryProposal()
  const archive = useArchiveMemoryProposal()
  const [note, setNote] = useState('')
  const [expanded, setExpanded] = useState(false)

  const decide = async (status: 'accepted' | 'rejected') => {
    try {
      await review.mutateAsync({ id: proposal.id, status, reviewNote: note.trim() || undefined })
      toast.success(status === 'accepted' ? 'Proposal accepted — apply it to create the memory' : 'Proposal rejected')
      setNote('')
    } catch (error) {
      toast.error(`Failed to review proposal: ${error instanceof Error ? error.message : 'unknown error'}`)
    }
  }

  const handleApply = async () => {
    try {
      const memory = await apply.mutateAsync({ id: proposal.id })
      toast.success(`Applied — memory ${memory.id} created`)
    } catch (error) {
      toast.error(`Failed to apply proposal: ${error instanceof Error ? error.message : 'unknown error'}`)
    }
  }

  const handleArchive = async () => {
    try {
      await archive.mutateAsync(proposal.id)
      toast.success('Proposal archived')
    } catch (error) {
      toast.error(`Failed to archive proposal: ${error instanceof Error ? error.message : 'unknown error'}`)
    }
  }

  const origin = [
    proposal.agentName && `agent ${proposal.agentName}`,
    proposal.taskName && `task ${proposal.taskName}`,
  ].filter(Boolean).join(' · ')

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="flex items-start justify-between gap-2 text-base">
          <span>{proposal.title}</span>
          <span className="flex shrink-0 items-center gap-1.5">
            {proposal.type && <Badge variant="secondary">{proposal.type}</Badge>}
            <Badge variant="secondary" className={statusBadgeClass(proposal.status)}>{proposal.status}</Badge>
          </span>
        </CardTitle>
        <CardDescription>
          {origin || 'manual proposal'} · {new Date(proposal.createdAt).toLocaleString()}
          {proposal.reviewer && ` · reviewed by ${proposal.reviewer}`}
          {proposal.appliedMemoryId && ` · memory ${proposal.appliedMemoryId}`}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {proposal.description && <p className="text-sm text-muted-foreground">{proposal.description}</p>}
        {(proposal.content || proposal.patch) && (
          <div>
            <button
              type="button"
              onClick={() => setExpanded((v) => !v)}
              className="text-sm font-medium text-primary hover:underline"
            >
              {expanded ? 'Hide proposed content' : 'Show proposed content'}
            </button>
            {expanded && (
              <div className="mt-2 space-y-2">
                {proposal.content && (
                  <pre className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs leading-5 whitespace-pre-wrap break-words">
                    {proposal.content}
                  </pre>
                )}
                {proposal.patch && (
                  <pre className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs leading-5 whitespace-pre-wrap break-words">
                    {proposal.patch}
                  </pre>
                )}
              </div>
            )}
          </div>
        )}
        {proposal.reviewNote && (
          <p className="text-sm"><span className="text-muted-foreground">Review note:</span> {proposal.reviewNote}</p>
        )}
        {proposal.status === 'pending' && (
          <div className="flex flex-wrap items-center gap-2">
            <Input
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder="Review note (optional)"
              aria-label="Review note"
              className="h-8 w-64"
            />
            <Button size="sm" onClick={() => decide('accepted')} disabled={review.isPending}>
              Accept
            </Button>
            <Button size="sm" variant="outline" onClick={() => decide('rejected')} disabled={review.isPending}>
              Reject
            </Button>
          </div>
        )}
        {proposal.status === 'accepted' && (
          <div className="flex items-center gap-2">
            <Button size="sm" onClick={handleApply} disabled={apply.isPending}>
              Apply to memory
            </Button>
            <Button size="sm" variant="outline" onClick={handleArchive} disabled={archive.isPending}>
              Archive
            </Button>
          </div>
        )}
        {proposal.status === 'rejected' && (
          <Button size="sm" variant="outline" onClick={handleArchive} disabled={archive.isPending}>
            Archive
          </Button>
        )}
      </CardContent>
    </Card>
  )
}

export function ProposalInbox() {
  const [status, setStatus] = useState<(typeof STATUS_FILTERS)[number]>('pending')
  const { data, isLoading, error } = useMemoryProposalList(
    status === 'all' ? {} : { status },
  )

  if (error && isMemoryStoreUnavailable(error)) {
    return (
      <EmptyState
        icon={Inbox}
        headline="Proposal store is not enabled"
        hint="Proposals appear here once the controller memory store is configured."
      />
    )
  }

  const items = data?.items ?? []

  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Accepting records a decision. Applying an accepted proposal is the separate, explicit step that creates the memory.
      </p>
      <div className="flex flex-wrap gap-1.5" role="group" aria-label="Filter by status">
        {STATUS_FILTERS.map((value) => (
          <button
            key={value}
            type="button"
            onClick={() => setStatus(value)}
            className={cn(
              'rounded-full border px-3 py-1 text-sm capitalize transition-colors',
              status === value
                ? 'border-primary bg-primary/10 font-medium text-primary'
                : 'border-border text-muted-foreground hover:bg-accent hover:text-accent-foreground',
            )}
          >
            {value}
          </button>
        ))}
      </div>
      {isLoading ? (
        <div className="space-y-3">
          <Skeleton className="h-32 w-full" />
          <Skeleton className="h-32 w-full" />
        </div>
      ) : items.length === 0 ? (
        <EmptyState
          icon={Inbox}
          headline={status === 'pending' ? 'No proposals waiting for review' : `No ${status === 'all' ? '' : status} proposals`}
          hint="Agents file proposals through the remember and propose_memory tools."
        />
      ) : (
        <div className="space-y-3">
          {items.map((proposal) => (
            <ProposalCard key={proposal.id} proposal={proposal} />
          ))}
        </div>
      )}
    </div>
  )
}
