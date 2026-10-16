import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { containsCredential, looksSecret, UNSAFE_KEY_PATTERN } from '@/lib/secret-like'
import { cn } from '@/lib/utils'
import type { Task } from '@/schemas/task'

interface TaskStatusDetailsProps {
  task: Task
}

function nonSecretEntries(record?: Record<string, string>): [string, string][] {
  if (!record) return []
  // Secret-looking or long values are dropped, as are values that show a
  // credential sign the YAML view would hide (a signed URL, say), and kubectl's
  // annotations.
  return Object.entries(record).filter(([k, v]) => !UNSAFE_KEY_PATTERN.test(k) && !looksSecret(k, v) && !containsCredential(v))
}

/** A collapsible, read-only section. Rendered only when it has content. */
function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <details open className="rounded-md border border-border/60">
      <summary className="cursor-pointer select-none px-3 py-2 text-xs font-medium text-muted-foreground">
        {title}
      </summary>
      <div className="px-3 pb-3 pt-1 text-xs">{children}</div>
    </details>
  )
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 py-0.5">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className="min-w-0 break-all text-right font-mono">{value}</span>
    </div>
  )
}

/**
 * The Task resource's status beyond its phase: conditions, the execution
 * workspace it was placed in, session and result references, and its labels and
 * annotations, with secret-looking ones filtered out.
 */
export function TaskStatusDetails({ task }: TaskStatusDetailsProps) {
  const { status, spec, metadata } = task
  const conditions = status?.conditions ?? []
  const ws = status?.executionWorkspace
  const sessionName = spec.sessionRef?.name
  const resultRef = status?.resultRef
  const legacyResultAvailable = resultRef?.available === undefined && Boolean(resultRef?.configMapName || resultRef?.key)
  const resultAvailable = resultRef?.available === true || legacyResultAvailable
  const labels = nonSecretEntries(metadata.labels)
  const annotations = nonSecretEntries(metadata.annotations)
  const wsPlacement = ws?.placement
  const wsDensity = ws?.density
  const hasLabels = labels.length > 0 || annotations.length > 0
  if (!conditions.length && !ws && !sessionName && !resultRef && !hasLabels) return null

  return (
    <Card>
      <CardHeader>
        <CardTitle>Status details</CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {conditions.length > 0 && (
          <Section title="Conditions">
            <ul className="space-y-1.5">
              {conditions.map((c, i) => (
                <li key={`${c.type}-${i}`}>
                  {/* Messages can carry long unbroken IDs (approval digests);
                      they wrap anywhere so the card keeps its width and the
                      status badge stays beside the type. */}
                  <div className="flex items-center gap-1.5">
                    <span className="min-w-0 wrap-anywhere font-mono">{c.type}</span>
                    <Badge className="shrink-0" variant={c.status === 'True' ? 'secondary' : 'outline'}>{c.status}</Badge>
                  </div>
                  {c.message && <p className="wrap-anywhere text-muted-foreground">{c.message}</p>}
                </li>
              ))}
            </ul>
          </Section>
        )}

        {ws && (
          <Section title="Execution workspace">
            {ws.provider && <Field label="Provider" value={ws.provider} />}
            {ws.phase && <Field label="Phase" value={ws.phase} />}
            {ws.reused !== undefined && <Field label="Reused" value={String(ws.reused)} />}
            {wsPlacement?.workerNamespace && <Field label="Namespace" value={wsPlacement.workerNamespace} />}
            {wsPlacement?.workerPool && <Field label="Worker pool" value={wsPlacement.workerPool} />}
            {wsPlacement?.workerPodName && <Field label="Worker pod" value={wsPlacement.workerPodName} />}
            {wsDensity?.workerCount !== undefined && <Field label="Workers" value={wsDensity.workerCount} />}
            {wsDensity?.actorCount !== undefined && <Field label="Actors" value={wsDensity.actorCount} />}
            {wsDensity?.runningActorCount !== undefined && <Field label="Running actors" value={wsDensity.runningActorCount} />}
            {wsDensity?.suspendedActorCount !== undefined && <Field label="Suspended actors" value={wsDensity.suspendedActorCount} />}
            {wsDensity?.actorsPerWorker && <Field label="Actors/worker" value={wsDensity.actorsPerWorker} />}
            {ws.resumeLatency && <Field label="Resume latency" value={ws.resumeLatency} />}
            {ws.message && <Field label="Message" value={ws.message} />}
          </Section>
        )}

        {sessionName && (
          <Section title="Session">
            <Field label="Session ref" value={sessionName} />
          </Section>
        )}

        {resultRef && (
          <Section title="Result">
            <Field label="Available" value={resultAvailable ? 'Yes' : 'No'} />
            {resultRef.configMapName && <Field label="ConfigMap" value={resultRef.configMapName} />}
            {resultRef.key && <Field label="Key" value={resultRef.key} />}
          </Section>
        )}

        {hasLabels && (
          <Section title="Labels and annotations">
            <dl className={cn('grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5')}>
              {[...labels, ...annotations].map(([k, v]) => (
                <div key={k} className="contents">
                  <dt className="truncate text-muted-foreground">{k}</dt>
                  <dd className="truncate font-mono">{v}</dd>
                </div>
              ))}
            </dl>
          </Section>
        )}
      </CardContent>
    </Card>
  )
}
