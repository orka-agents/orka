import { Info, TriangleAlert, CircleAlert, Bug, type LucideIcon } from 'lucide-react'
import { normalizeSeverity, type ExecutionEventSeverityLevel } from '@/lib/execution-events'

export interface SeverityMeta {
  label: string
  icon: LucideIcon
  className: string
}

// Severity is a distinct visual axis from phase/type — it describes an event's
// importance, not a task's lifecycle. It still draws from the same deep-ocean
// status palette so the UI speaks one color language: info→running (cyan),
// warning→pending (amber), error→failed (rose). Kept in a plain .ts module (no
// JSX) so the non-component export doesn't trip react-refresh, mirroring
// lib/task-status.ts.
const SEVERITY_META: Record<ExecutionEventSeverityLevel, SeverityMeta> = {
  debug: { label: 'Debug', icon: Bug, className: 'text-muted-foreground' },
  info: { label: 'Info', icon: Info, className: 'text-status-running' },
  warning: { label: 'Warning', icon: TriangleAlert, className: 'text-status-pending' },
  error: { label: 'Error', icon: CircleAlert, className: 'text-status-failed' },
}

export function severityMeta(severity?: string): SeverityMeta {
  return SEVERITY_META[normalizeSeverity(severity)]
}
