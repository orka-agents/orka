import { Cpu } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ListAccessError } from '@/components/ui/list-access-error'
import { Skeleton } from '@/components/ui/skeleton'
import { useOversightProducers } from '@/hooks/use-oversight'
import { cn } from '@/lib/utils'
import type { ProducerInfo, ProducerMetric } from '@/schemas/oversight'
import { Chip } from './signal'

/** Below this many samples a declared metric is flagged as a small sample. */
const SMALL_SAMPLE = 100

function formatMetricValue(value: number) {
  if (!Number.isFinite(value)) return '?'
  return Number.isInteger(value) ? String(value) : value.toFixed(Math.abs(value) < 1 ? 2 : 1)
}

/** "0.92 AUROC · run r1 · n=26": every number carries its run and sample size. */
function metricText(metric: ProducerMetric, producer: ProducerInfo) {
  const value = `${formatMetricValue(metric.value)}${metric.unit ? ` ${metric.unit}` : ''} ${metric.name}`
  const run = producer.evalRunID ? `run ${producer.evalRunID}` : 'run not declared'
  const n = producer.sampleSize ? `n=${producer.sampleSize}` : 'n not declared'
  return `${value} · ${run} · ${n}`
}

function ProducerRow({ producer }: { producer: ProducerInfo }) {
  const small = producer.sampleSize !== undefined && producer.sampleSize > 0 && producer.sampleSize < SMALL_SAMPLE
  const failureRate = producer.evaluations > 0 ? producer.failures / producer.evaluations : 0
  return (
    <li className="space-y-1.5 py-3 first:pt-0 last:pb-0" data-testid="producer-row">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-sm font-medium break-all">{producer.label?.trim() || producer.providerRef}</span>
        {producer.role && <Chip className="text-muted-foreground">{producer.role}</Chip>}
      </div>
      {(producer.modelID || producer.modelDigest) && (
        <p className="font-mono text-xs break-all text-muted-foreground">
          {producer.modelID}{producer.modelFamily && ` (${producer.modelFamily})`}
          {producer.modelDigest && <span title={producer.modelDigest}> · {producer.modelDigest.slice(0, 19)}</span>}
        </p>
      )}
      <div className="flex flex-wrap items-center gap-1.5 text-xs">
        {producer.declaredTrustState ? (
          <Chip className={cn(producer.declaredTrustState === 'untrusted_monitor' && 'border-status-failed/50 text-status-failed')}>
            {producer.declaredTrustState}
            <span className="text-muted-foreground">{producer.trustVerified ? '(verified by Orka)' : '(declared, not verified by Orka)'}</span>
          </Chip>
        ) : (
          <Chip className="text-muted-foreground">no trust state declared</Chip>
        )}
        {(producer.observedTrustStates?.length ?? 0) > 0 && (
          <span className="text-muted-foreground">decisions reported: {producer.observedTrustStates!.join(', ')}</span>
        )}
      </div>
      {(producer.metrics?.length ?? 0) > 0 ? (
        <ul className="space-y-0.5 text-xs" aria-label="Declared metrics">
          {producer.metrics!.map((metric) => (
            <li key={metric.name} className="font-mono tabular-nums">{metricText(metric, producer)}</li>
          ))}
        </ul>
      ) : (
        <p className="text-xs text-muted-foreground">
          No metrics declared{producer.evalRunID ? ` · run ${producer.evalRunID}` : ''}{producer.sampleSize ? ` · n=${producer.sampleSize}` : ''}
        </p>
      )}
      {small && <p className="text-xs font-medium text-status-pending">Small sample: n={producer.sampleSize}</p>}
      <p className={cn('text-xs tabular-nums', producer.failures > 0 ? 'text-status-failed' : 'text-muted-foreground')}>
        Last 24 hours: {producer.evaluations} evaluations, {producer.failures} failures
        {producer.failures > 0 && ` (${Math.round(failureRate * 100)}%)`}
      </p>
      {producer.note && <p className="text-xs whitespace-pre-wrap break-words text-muted-foreground">{producer.note}</p>}
    </li>
  )
}

/** The scorers, advisors and rule producers behind the findings. */
export function ProducersPanel() {
  const { data, error, isLoading } = useOversightProducers()
  const producers = data?.producers ?? []
  return (
    <Card role="region" aria-label="Scorers">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <Cpu className="size-4" aria-hidden="true" /> Scorers
        </CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : error ? (
          <ListAccessError error={error} resource="oversight producers" />
        ) : producers.length === 0 ? (
          <p className="text-sm text-muted-foreground">No producer declarations.</p>
        ) : (
          <>
            {!data?.declared && (
              <p className="mb-2 text-xs text-muted-foreground">No operator declaration found: these were only seen in decisions.</p>
            )}
            <ul className="divide-y">
              {producers.map((producer) => <ProducerRow key={`${producer.role}-${producer.providerRef}`} producer={producer} />)}
            </ul>
          </>
        )}
      </CardContent>
    </Card>
  )
}
