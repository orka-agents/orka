import { Button } from '@/components/ui/button'

interface TaskInventoryErrorProps {
  isRetrying: boolean
  onRetry: () => void
}

export function TaskInventoryError({
  isRetrying,
  onRetry,
}: TaskInventoryErrorProps) {
  return (
    <div
      role="alert"
      aria-label="Unable to load complete task inventory"
      className="flex flex-col items-start gap-3 rounded-md border border-destructive/40 bg-destructive/10 p-4"
    >
      <div>
        <p className="text-sm font-medium text-destructive">
          Unable to load complete task inventory.
        </p>
        <p className="text-xs text-muted-foreground">
          Task counts and views may be incomplete until pagination succeeds.
        </p>
      </div>
      <Button
        variant="outline"
        size="sm"
        onClick={onRetry}
        disabled={isRetrying}
      >
        {isRetrying ? 'Retrying task inventory…' : 'Retry task inventory'}
      </Button>
    </div>
  )
}
