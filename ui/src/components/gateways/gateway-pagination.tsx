import { ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'

export function GatewayLedgerPagination({
  label,
  page,
  hasPrevious,
  nextCursor,
  onPrevious,
  onNext,
}: {
  label: string
  page: number
  hasPrevious: boolean
  nextCursor?: string
  onPrevious: () => void
  onNext: (cursor: string) => void
}) {
  return (
    <div className="flex items-center justify-end gap-2" aria-label={`${label} pagination`}>
      <span className="mr-2 text-xs text-muted-foreground">Page {page} · up to 100 records</span>
      <Button
        type="button"
        size="sm"
        variant="outline"
        aria-label={`Previous ${label} page`}
        disabled={!hasPrevious}
        onClick={onPrevious}
      >
        <ChevronLeft className="h-4 w-4" />
        Previous
      </Button>
      <Button
        type="button"
        size="sm"
        variant="outline"
        aria-label={`Next ${label} page`}
        disabled={!nextCursor}
        onClick={() => nextCursor && onNext(nextCursor)}
      >
        Next
        <ChevronRight className="h-4 w-4" />
      </Button>
    </div>
  )
}
