import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { StatusDot } from '@/components/ui/status-dot'
import { cn } from '@/lib/utils'
import type { Task } from '@/schemas/task'

interface LiveStatePanelProps {
  task: Task
}

// Skip keys that look credential-bearing OR are known large/spec-bearing
// annotations (kubectl last-applied embeds the full spec, including env).
const SECRET_KEY_PATTERN = /token|secret|key|password|cred|auth/i
const UNSAFE_KEY_PATTERN = /last-applied-configuration|kubectl\.kubernetes\.io/i
// Values long enough to plausibly carry an embedded blob/token are dropped too.
const MAX_VALUE_CHARS = 80

function nonSecretEntries(record?: Record<string, string>): [string, string][] {
  if (!record) return []
  return Object.entries(record).filter(
    ([k, v]) => !SECRET_KEY_PATTERN.test(k) && !UNSAFE_KEY_PATTERN.test(k) && v.length <= MAX_VALUE_CHARS,
  )
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
      <span className="text-muted-foreground">{label}</span>
      <span className="font-mono">{value}</span>
    </div>
  )
}

/**
 * Read-only inspector for a Task's live runtime state. Renders typed status,
 * conditions, child tasks, execution-workspace placement/density, session and
 * result references. Mutating affordances are deliberately absent — this is an
 * observation surface only — and secret-ish labels/annotations are filtered.
 */
export function LiveStatePanel({ task }: LiveStatePanelProps) {
  const { status, spec, metadata } = task
  const conditions = status?.conditions ?? []
  const children = status?.childTasks ?? []
  const ws = status?.executionWorkspace
  const sessionName = spec.sessionRef?.name
  const resultRef = status?.resultRef
  const labels = nonSecretEntries(metadata.labels)
  const annotations = nonSecretEntries(metadata.annotations)
  const wsPlacement = ws?.placement
  const wsDensity = ws?.density

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          Live state
          <Badge variant="outline">read-only</Badge>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2 pt-0">
        <Section title="Status">
          <Field label="Phase" value={<StatusDot phase={status?.phase} />} />
          {status?.message && <Field label="Message" value={status.message} />}
        </Section>

        {conditions.length > 0 && (
          <Section title="Conditions">
            <ul className="space-y-1.5">
              {conditions.map((c, i) => (
                <li key={`${c.type}-${i}`}>
                  <div className="flex items-center gap-1.5">
                    <span className="font-mono">{c.type}</span>
                    <Badge variant={c.status === 'True' ? 'secondary' : 'outline'}>{c.status}</Badge>
                  </div>
                  {c.message && <p className="text-muted-foreground">{c.message}</p>}
                </li>
              ))}
            </ul>
          </Section>
        )}

        {children.length > 0 && (
          <Section title="Child tasks">
            <ul className="space-y-1.5">
              {children.map((c) => (
                <li key={c.name} className="flex items-center justify-between gap-2">
                  <span className="truncate font-mono">{c.name}</span>
                  <span className="flex items-center gap-1.5">
                    <Badge variant="outline">{c.agent}</Badge>
                    <StatusDot phase={c.phase} hideLabel />
                    {c.result && <span className="text-muted-foreground">{c.result}</span>}
                  </span>
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
            <Field label="Available" value={resultRef.available ? 'Yes' : 'No'} />
            {resultRef.configMapName && <Field label="ConfigMap" value={resultRef.configMapName} />}
            {resultRef.key && <Field label="Key" value={resultRef.key} />}
          </Section>
        )}

        {(labels.length > 0 || annotations.length > 0) && (
          <Section title="Metadata">
            <dl className={cn('grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5')}>
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
