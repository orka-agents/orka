import { severityMeta } from '@/lib/event-severity'

export function SeverityIcon({ severity, className }: { severity?: string; className?: string }) {
  const meta = severityMeta(severity)
  const Icon = meta.icon
  return (
    <span className={`inline-flex items-center ${meta.className} ${className ?? ''}`}>
      <Icon className="h-4 w-4" aria-hidden="true" />
      <span className="sr-only">{meta.label}</span>
    </span>
  )
}
