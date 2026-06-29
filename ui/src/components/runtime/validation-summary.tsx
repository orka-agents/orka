import { CheckCircle2, XCircle, AlertTriangle, HelpCircle } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import {
  deriveRuntimeChecks,
  rollupStatus,
  type RuntimeCheck,
  type CheckStatus,
} from '@/lib/runtime-validation'
import type { Task } from '@/schemas/task'
import type { TaskTrace, Approval } from '@/schemas/execution-event'
import type { ArtifactMetadata } from '@/schemas/artifact'

interface ValidationSummaryProps {
  task: Task
  trace?: TaskTrace
  approvals?: Approval[]
  artifacts?: ArtifactMetadata[]
}

// Status presentation is text + icon, never color alone. Each label is rendered
// (or carried via sr-only) so screen readers and colorblind users get the state.
const STATUS_META: Record<
  CheckStatus,
  { Icon: typeof CheckCircle2; label: string; color: string; variant: 'default' | 'secondary' | 'destructive' | 'outline' }
> = {
  pass: { Icon: CheckCircle2, label: 'Pass', color: 'text-status-succeeded', variant: 'secondary' },
  fail: { Icon: XCircle, label: 'Fail', color: 'text-status-failed', variant: 'destructive' },
  warn: { Icon: AlertTriangle, label: 'Warn', color: 'text-status-pending', variant: 'secondary' },
  unknown: { Icon: HelpCircle, label: 'Unknown', color: 'text-muted-foreground', variant: 'outline' },
}

const ROLLUP_LABEL: Record<CheckStatus, string> = {
  pass: 'All derived checks pass',
  fail: 'Derived checks failing',
  warn: 'Derived checks need attention',
  unknown: 'Derived health unknown',
}

/**
 * Derived runtime health summary. This surfaces health inferred from real
 * Task/trace data and is explicitly NOT a formal evaluation — missing data
 * reads as "unknown", never a false pass. Pure props; no fetching.
 */
export function ValidationSummary({ task, trace, approvals, artifacts }: ValidationSummaryProps) {
  const checks = deriveRuntimeChecks({ task, trace, approvals, artifacts })
  const rollup = rollupStatus(checks)
  const banner = STATUS_META[rollup]

  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between gap-2">
          <CardTitle className="text-sm font-medium">Derived checks</CardTitle>
          <Badge variant={banner.variant} className={cn('gap-1', rollup !== 'fail' && banner.color)}>
            <banner.Icon className="size-3" aria-hidden="true" />
            {ROLLUP_LABEL[rollup]}
          </Badge>
        </div>
        <p className="text-xs text-muted-foreground">Not a formal evaluation</p>
      </CardHeader>
      <CardContent className="pt-0">
        <ul className="space-y-2">
          {checks.map((check) => (
            <CheckRow key={check.id} check={check} />
          ))}
        </ul>
      </CardContent>
    </Card>
  )
}

function CheckRow({ check }: { check: RuntimeCheck }) {
  const meta = STATUS_META[check.status]
  return (
    <li className="flex items-start gap-2 text-sm">
      <meta.Icon className={cn('mt-0.5 size-4 shrink-0', meta.color)} aria-hidden="true" />
      <div className="min-w-0 flex-1">
        <span className="font-medium">{check.label}</span>
        <span className="sr-only"> — {meta.label}</span>
        <span className="ml-2 text-xs text-muted-foreground">{check.reason}</span>
      </div>
    </li>
  )
}
