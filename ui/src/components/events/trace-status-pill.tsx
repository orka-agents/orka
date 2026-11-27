import { Badge } from '@/components/ui/badge'

function statusToken(status?: string): { className: string; live: boolean; label: string } {
  switch ((status ?? '').toLowerCase()) {
    case 'running':
      return { className: 'text-status-running', live: true, label: status ?? 'running' }
    case 'completed':
    case 'succeeded':
      return { className: 'text-status-succeeded', live: false, label: status ?? 'completed' }
    case 'failed':
      return { className: 'text-status-failed', live: false, label: status ?? 'failed' }
    case 'cancelled':
      return { className: 'text-muted-foreground', live: false, label: status ?? 'cancelled' }
    default:
      return { className: 'text-muted-foreground', live: false, label: status || 'unknown' }
  }
}

export function TraceStatusPill({ status }: { status?: string }) {
  const { className, live, label } = statusToken(status)
  return (
    <Badge variant="outline" className={`gap-1.5 ${className}`}>
      <span
        aria-hidden="true"
        className={`inline-block size-1.5 rounded-full bg-current ${live ? 'motion-safe:animate-pulse-live' : ''}`}
      />
      {label}
    </Badge>
  )
}
