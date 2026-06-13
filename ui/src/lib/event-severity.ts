import { Info, TriangleAlert, CircleAlert, Bug, type LucideIcon } from 'lucide-react'
import { normalizeSeverity, type ExecutionEventSeverityLevel } from '@/lib/execution-events'

export interface SeverityMeta {
  label: string
  icon: LucideIcon
  className: string
}

// Severity is a distinct visual axis from phase/type — it describes an event's
// importance, not a task's lifecycle. Kept in a plain .ts module (no JSX) so the
// non-component export doesn't trip react-refresh, mirroring lib/task-status.ts.
const SEVERITY_META: Record<ExecutionEventSeverityLevel, SeverityMeta> = {
  debug: { label: 'Debug', icon: Bug, className: 'text-muted-foreground' },
  info: { label: 'Info', icon: Info, className: 'text-blue-600 dark:text-blue-400' },
  warning: { label: 'Warning', icon: TriangleAlert, className: 'text-yellow-600 dark:text-yellow-400' },
  error: { label: 'Error', icon: CircleAlert, className: 'text-red-600 dark:text-red-400' },
}

export function severityMeta(severity?: string): SeverityMeta {
  return SEVERITY_META[normalizeSeverity(severity)]
}
