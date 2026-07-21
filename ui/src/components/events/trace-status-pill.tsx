import { Badge } from '@/components/ui/badge'

// Map a trace item status (running / completed / succeeded / failed / cancelled
// / phase strings) onto the shared deep-ocean status palette. Status is an
// encoding axis, so it draws from the same tokens as task phases.
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
    case 'skipped':
      return { className: 'text-muted-foreground', live: false, label: status ?? 'cancelled' }
    default:
      return { className: 'text-muted-foreground', live: false, label: status || 'unknown' }
  }
}

// A compact, accessible status pill: a colored dot (with a motion-safe pulse for
// live states) plus a text label, so status is never conveyed by color alone.
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
