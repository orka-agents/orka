import { Info, TriangleAlert, CircleAlert, Bug, type LucideIcon } from 'lucide-react'
import { normalizeSeverity, type ExecutionEventSeverityLevel } from '@/lib/execution-events'

export interface SeverityMeta {
  label: string
  icon: LucideIcon
  className: string
}

const SEVERITY_META: Record<ExecutionEventSeverityLevel, SeverityMeta> = {
  debug: { label: 'Debug', icon: Bug, className: 'text-muted-foreground' },
  info: { label: 'Info', icon: Info, className: 'text-status-running' },
  warning: { label: 'Warning', icon: TriangleAlert, className: 'text-status-pending' },
  error: { label: 'Error', icon: CircleAlert, className: 'text-status-failed' },
}

export function severityMeta(severity?: string): SeverityMeta {
  return SEVERITY_META[normalizeSeverity(severity)]
}
