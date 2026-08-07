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
import type { AgentExecutionResolutionRef, Task } from '@/schemas/task'
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
  const quarantine = status?.agentExecutionQuarantine
  const noExecution = status?.agentExecutionNoExecution
  const resolution = status?.agentExecutionResolutionRef
  const outcomeUnknown = status?.execution?.outcome === 'OutcomeUnknown'
    || status?.execution?.state === 'OutcomeUnknown'
    || status?.harnessRuntime?.outcome === 'OutcomeUnknown'
    || status?.harnessRuntime?.state === 'OutcomeUnknown'

  if (quarantine) {
    return (
      <ExecutionRouteLedger
        title="Execution route"
        summary={resolution ? 'Resolution applied' : 'Reconciliation required'}
        summaryTone={resolution ? 'warning' : 'danger'}
        rows={[
          {
            label: 'Route',
            title: 'Execution held in quarantine',
            detail: quarantine.reason,
            icon: Route,
            tone: 'danger',
          },
          {
            label: 'Evidence',
            title: 'Immutable migration evidence',
            detail: (
              <div className="space-y-1">
                <p>{quarantine.migrationInventoryID}</p>
                {quarantine.v1EvidenceDigest && (
                  <p>v1 <EvidenceValue value={quarantine.v1EvidenceDigest} /></p>
                )}
                {quarantine.v2EvidenceDigest && (
                  <p>v2 <EvidenceValue value={quarantine.v2EvidenceDigest} /></p>
                )}
              </div>
            ),
            icon: Database,
            tone: 'warning',
          },
          resolutionRow(resolution),
        ]}
      />
    )
  }

  if (noExecution) {
    return (
      <ExecutionRouteLedger
        title="Execution route"
        summary="No executor state"
        summaryTone="neutral"
        rows={[
          {
            label: 'Route',
            title: 'Common cleanup only',
            detail: noExecution.state,
            icon: Route,
          },
          {
            label: 'Evidence',
            title: 'No-route proof recorded',
            detail: noExecution.migrationInventoryID,
            mono: noExecution.evidenceDigest,
            icon: Database,
          },
          resolution
            ? resolutionRow(resolution)
            : {
                label: 'Reconciliation',
                title: 'No operator decision required',
                detail: 'Authoritative inventory proved no executor accepted this Task.',
                icon: CircleCheck,
                tone: 'good',
              },
        ]}
      />
    )
  }

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

  const control = binding.backendControl
  const snapshotDetail = binding.runtimeProfileDigest
    ? <>Snapshot and runtime profile pinned</>
    : <>Snapshot retained while this binding is referenced</>

  return (
    <ExecutionRouteLedger
      title="Execution route"
      summary={outcomeUnknown ? (resolution ? 'Resolution applied' : 'Outcome unknown') : 'Route locked'}
      summaryTone={outcomeUnknown ? (resolution ? 'warning' : 'danger') : 'good'}
      rows={[
        {
          label: 'Route',
          title: bindingRouteTitle(task),
          detail: (
            <span className="capitalize">
              {binding.mode} · {words(binding.provenance)}
            </span>
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
        resolution
          ? resolutionRow(resolution)
          : outcomeUnknown
          ? {
              label: 'Reconciliation',
              title: 'Human reconciliation required',
              detail: status?.execution?.reason ?? status?.execution?.message,
              icon: ShieldAlert,
              tone: 'danger',
            }
          : {
              label: 'Admission',
              title: control
                ? `${control.admittedMode} · revision ${control.modeRevision}`
                : 'Legacy admission evidence',
              detail: binding.policy
                ? <>Policy <EvidenceValue value={binding.policy.digest} /></>
                : 'No compatibility policy required for this route',
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
  const resolution = control.agentExecutionResolutionRef
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
          ? resolution
            ? 'Resolution applied'
            : 'Reconciliation required'
          : available
            ? 'Available'
            : 'Availability unknown'
      }
      summaryTone={blocked ? (resolution ? 'warning' : 'danger') : available ? 'good' : 'warning'}
      rows={[
        {
          label: 'Route',
          title: routeTitle,
          detail: lineage
            ? `${lineage.runtimeIdentity} · ${words(lineage.provenance)}`
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
              title: resolution ? 'Operator resolution recorded' : 'Continuation is blocked',
              detail: resolution
                ? `${resolution.action} · ${resolution.adjudicationName}`
                : control.blockedReason,
              mono: resolution?.resolutionDigest,
              icon: ShieldAlert,
              tone: resolution ? 'warning' : 'danger',
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

function resolutionRow(resolution?: AgentExecutionResolutionRef): LedgerRow {
  if (!resolution) {
    return {
      label: 'Reconciliation',
      title: 'Operator decision required',
      detail: 'Execution remains held until evidence is adjudicated.',
      icon: ShieldAlert,
      tone: 'danger',
    }
  }
  return {
    label: 'Reconciliation',
    title: 'Operator resolution recorded',
    detail: `${resolution.action} · ${resolution.adjudicationName}`,
    mono: resolution.resolutionDigest,
    icon: ShieldAlert,
    tone: 'warning',
  }
}
