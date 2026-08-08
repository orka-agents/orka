import type { ReactNode } from 'react'
import {
  CircleAlert,
  CircleCheck,
  Database,
  GitBranch,
  Route,
  ShieldAlert,
} from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import type { Task } from '@/schemas/task'
import type { Session } from '@/schemas/session'
import { abbreviateEvidence, words } from './execution-route-format'

type LedgerTone = 'neutral' | 'good' | 'warning' | 'danger'

interface LedgerRow {
  label: string
  title: string
  detail?: ReactNode
  mono?: string
  icon: typeof Route
  tone?: LedgerTone
}

interface LedgerProps {
  title: string
  summary: string
  summaryTone: LedgerTone
  rows: LedgerRow[]
}

const toneClasses: Record<LedgerTone, string> = {
  neutral: 'border-border bg-muted/40 text-muted-foreground',
  good: 'border-status-succeeded/30 bg-status-succeeded-bg text-status-succeeded',
  warning: 'border-status-pending/30 bg-status-pending-bg text-status-pending',
  danger: 'border-status-failed/30 bg-status-failed-bg text-status-failed',
}

function EvidenceValue({ value }: { value: string }) {
  return (
    <span className="font-mono text-[11px] text-foreground">
      {abbreviateEvidence(value)}
    </span>
  )
}

function ExecutionRouteLedger({ title, summary, summaryTone, rows }: LedgerProps) {
  return (
    <Card aria-label={`${title} evidence`}>
      <CardHeader className="border-b pb-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.18em] text-muted-foreground">
              Route / evidence / reconciliation
            </p>
            <CardTitle>{title}</CardTitle>
          </div>
          <Badge variant="outline" className={cn('border', toneClasses[summaryTone])}>
            {summary}
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="pt-5">
        <ol className="grid gap-0 md:grid-cols-3" aria-label="Execution route evidence">
          {rows.map((row, index) => {
            const Icon = row.icon
            return (
              <li
                key={`${row.label}-${index}`}
                className="relative border-l border-border pb-5 pl-9 last:pb-0 md:border-l-0 md:border-t md:pb-0 md:pl-0 md:pt-8 md:pr-6 md:last:pr-0"
              >
                <span
                  className={cn(
                    'absolute -left-[13px] top-0 flex size-6 items-center justify-center rounded-full border bg-card md:-top-[13px] md:left-0',
                    toneClasses[row.tone ?? 'neutral'],
                  )}
                  aria-hidden="true"
                >
                  <Icon className="size-3" />
                </span>
                <p className="font-mono text-[10px] uppercase tracking-[0.16em] text-muted-foreground">
                  {String(index + 1).padStart(2, '0')} · {row.label}
                </p>
                <p className="mt-1 text-sm font-medium">{row.title}</p>
                {row.detail && <div className="mt-1 text-xs text-muted-foreground">{row.detail}</div>}
                {row.mono && (
                  <div className="mt-1">
                    <EvidenceValue value={row.mono} />
                  </div>
                )}
              </li>
            )
          })}
        </ol>
      </CardContent>
    </Card>
  )
}

function bindingRouteTitle(task: Task) {
  const binding = task.status?.agentExecutionBinding
  if (!binding) return 'No execution route recorded'
  const contract = binding.contractVersion === 'orka.harness.v1' ? 'Harness v1' : 'ACP v2'
  const backend = words(binding.backend)
  return `${contract} · ${backend}`
}

export function TaskExecutionRouteLedger({ task }: { task: Task }) {
  if (task.spec.type !== 'agent') return null

  const status = task.status
  const binding = status?.agentExecutionBinding
  const outcomeUnknown = status?.execution?.outcome === 'OutcomeUnknown'
    || status?.execution?.state === 'OutcomeUnknown'
    || status?.harnessRuntime?.outcome === 'OutcomeUnknown'
    || status?.harnessRuntime?.state === 'OutcomeUnknown'

  if (!binding) {
    return (
      <ExecutionRouteLedger
        title="Execution route"
        summary="Awaiting binding"
        summaryTone="warning"
        rows={[
          { label: 'Route', title: 'Not yet bound', icon: Route, tone: 'warning' },
          { label: 'Evidence', title: 'No binding snapshot recorded', icon: Database },
          { label: 'Reconciliation', title: 'No conflict recorded', icon: CircleCheck },
        ]}
      />
    )
  }

  const snapshotDetail = binding.runtimeProfileDigest
    ? <>Snapshot and runtime profile pinned</>
    : <>Snapshot retained while this binding is referenced</>

  return (
    <ExecutionRouteLedger
      title="Execution route"
      summary={outcomeUnknown ? 'Outcome unknown' : 'Route locked'}
      summaryTone={outcomeUnknown ? 'danger' : 'good'}
      rows={[
        {
          label: 'Route',
          title: bindingRouteTitle(task),
          detail: (
            <span>{words(binding.backend)}</span>
          ),
          mono: binding.bindingDigest,
          icon: Route,
          tone: 'good',
        },
        {
          label: 'Evidence',
          title: `Snapshot schema v${binding.snapshot.schemaVersion}`,
          detail: snapshotDetail,
          mono: binding.snapshot.digest,
          icon: Database,
        },
        outcomeUnknown
          ? {
              label: 'Reconciliation',
              title: 'Human reconciliation required',
              detail: status?.execution?.reason
                || status?.execution?.message
                || status?.harnessRuntime?.reason
                || status?.harnessRuntime?.message,
              icon: ShieldAlert,
              tone: 'danger',
            }
          : {
              label: 'Admission',
              title: 'Static namespace route',
              detail: `Contract ${binding.contractVersion} is fixed by the installation mode`,
              icon: GitBranch,
              tone: 'good',
            },
      ]}
    />
  )
}

export function SessionExecutionRouteLedger({ session }: { session: Session }) {
  const control = session.executionControl
  if (!control) return null

  const lineage = control.lineage
  const blocked = control.availability === 'ReconciliationBlocked'
  const available = control.availability === 'Available'
  const routeTitle = lineage
    ? `${lineage.contractVersion === 'orka.harness.v1' ? 'Harness v1' : 'ACP v2'} · lineage ${lineage.generation}`
    : 'Lineage not yet established'

  return (
    <ExecutionRouteLedger
      title="Session lineage"
      summary={
        blocked
          ? 'Reconciliation required'
          : available
            ? 'Available'
            : 'Availability unknown'
      }
      summaryTone={blocked ? 'danger' : available ? 'good' : 'warning'}
      rows={[
        {
          label: 'Route',
          title: routeTitle,
          detail: lineage
            ? lineage.runtimeIdentity
            : `Lifecycle ${control.lifecycle ?? 'unclassified'}`,
          mono: lineage?.sessionUID ?? control.sessionUID,
          icon: Route,
          tone: lineage ? 'good' : 'warning',
        },
        {
          label: 'Evidence',
          title: lineage ? 'Immutable lineage claim' : 'Control record present',
          detail: control.runtimePoolRef
            ? `Runtime pool ${control.runtimePoolRef}`
            : `Control generation ${control.generation ?? 0}`,
          mono: lineage?.configDigest ?? control.runtimeProfileDigest,
          icon: Database,
        },
        blocked
          ? {
              label: 'Reconciliation',
              title: 'Continuation is blocked',
              detail: control.blockedReason,
              icon: ShieldAlert,
              tone: 'danger',
            }
          : {
              label: 'Availability',
              title: available ? 'Available' : 'Availability not yet established',
              detail: `Lifecycle ${control.lifecycle ?? 'unclassified'} · lease generation ${control.mutationLeaseGeneration ?? 0}`,
              icon: available ? CircleCheck : CircleAlert,
              tone: available ? 'good' : 'warning',
            },
      ]}
    />
  )
}
