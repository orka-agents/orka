import { Link } from '@tanstack/react-router'
import {
  Activity,
  Cpu,
  Wrench,
  ListTree,
  FolderGit2,
  Package,
  ShieldCheck,
  GitFork,
  CircleAlert,
  TriangleAlert,
  FileText,
} from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { EventRow } from './event-row'
import { TraceStatusPill } from './trace-status-pill'
import { executionEventCategory } from '@/lib/execution-events'
import type { TaskTrace, TraceEvent } from '@/schemas/execution-event'

function Section({
  icon: Icon,
  title,
  count,
  children,
}: {
  icon: typeof Activity
  title: string
  count?: number
  children: React.ReactNode
}) {
  return (
    <section className="space-y-2">
      <div className="flex items-center gap-2">
        <Icon className="h-4 w-4 text-muted-foreground" aria-hidden="true" />
        <h3 className="text-sm font-semibold">{title}</h3>
        {typeof count === 'number' && (
          <span className="text-xs text-muted-foreground">{count}</span>
        )}
      </div>
      {children}
    </section>
  )
}

function summaryText(event: TraceEvent): string {
  return event.summary || event.contentText || event.type
}

export interface TaskTraceViewProps {
  trace: TaskTrace
}

// Explainable, grouped view of a task's execution derived from its event stream.
// Approvals and fork provenance are not separate API arrays — they live in the
// timeline keyed by category — so we surface them by filtering the timeline.
export function TaskTraceView({ trace }: TaskTraceViewProps) {
  const approvalEvents = trace.timeline.filter((e) => executionEventCategory(e.type) === 'approvals')
  const forkEvents = trace.timeline.filter((e) => executionEventCategory(e.type) === 'fork')

  // A trace with no derived groups and no timeline falls back to a clear empty
  // state; a trace with a timeline but no structured groups still shows the raw
  // timeline so nothing is hidden. Errors and warnings render their own visible
  // sections, so an error- or warning-only trace also counts as structured.
  const hasStructured =
    trace.modelRequests.length > 0 ||
    trace.toolCalls.length > 0 ||
    trace.childTasks.length > 0 ||
    trace.workspace.length > 0 ||
    trace.artifacts.length > 0 ||
    trace.errors.length > 0 ||
    trace.warnings.length > 0 ||
    approvalEvents.length > 0 ||
    forkEvents.length > 0

  return (
    <div className="space-y-6" data-testid="task-trace">
      {/* Lifecycle summary */}
      <Section icon={Activity} title="Lifecycle">
        <div className="flex flex-wrap items-center gap-2 rounded-md border bg-card px-3 py-2 text-sm">
          <TraceStatusPill status={trace.task.phase} />
          {trace.task.type && <Badge variant="secondary" className="text-[10px]">{trace.task.type}</Badge>}
          {trace.task.agentName && (
            <span className="text-xs text-muted-foreground">agent: {trace.task.agentName}</span>
          )}
          {trace.task.sessionName && (
            <Link
              to="/sessions/$sessionId"
              params={{ sessionId: trace.task.sessionName }}
              className="text-xs text-primary hover:underline"
            >
              session: {trace.task.sessionName}
            </Link>
          )}
          {trace.task.resultAvailable && (
            <Badge variant="outline" className="gap-1 text-[10px]">
              <FileText className="h-3 w-3" /> result available
            </Badge>
          )}
          <span className="ml-auto font-mono text-xs text-muted-foreground">latest #{trace.latestSeq}</span>
        </div>
      </Section>

      {/* Errors — surfaced prominently above the structured detail. */}
      {trace.errors.length > 0 && (
        <Section icon={CircleAlert} title="Errors" count={trace.errors.length}>
          <ul className="space-y-1.5">
            {trace.errors.map((issue, i) => (
              <li
                key={`err-${i}`}
                className="flex items-start gap-2 rounded-md border border-status-failed/40 bg-status-failed-bg px-3 py-2 text-sm"
              >
                <CircleAlert className="mt-0.5 h-4 w-4 shrink-0 text-status-failed" aria-hidden="true" />
                <div className="min-w-0">
                  <p className="break-words">{issue.message}</p>
                  <p className="text-xs text-muted-foreground">
                    {issue.type}
                    {typeof issue.seq === 'number' && issue.seq > 0 ? ` · #${issue.seq}` : ''}
                  </p>
                </div>
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Model requests */}
      {trace.modelRequests.length > 0 && (
        <Section icon={Cpu} title="Model requests" count={trace.modelRequests.length}>
          <ul className="space-y-1.5">
            {trace.modelRequests.map((m) => (
              <li key={m.id} className="flex flex-wrap items-center gap-2 rounded-md border bg-card px-3 py-2 text-sm">
                <TraceStatusPill status={m.status} />
                {m.summary && <span className="min-w-0 break-words">{m.summary}</span>}
                {m.error && <span className="text-status-failed">{m.error}</span>}
                <span className="ml-auto font-mono text-xs text-muted-foreground">
                  {m.startSeq ? `#${m.startSeq}` : ''}{m.endSeq ? `–#${m.endSeq}` : ''}
                </span>
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Tool calls */}
      {trace.toolCalls.length > 0 && (
        <Section icon={Wrench} title="Tool calls" count={trace.toolCalls.length}>
          <ul className="space-y-1.5">
            {trace.toolCalls.map((t) => (
              <li key={t.id} className="flex flex-wrap items-center gap-2 rounded-md border bg-card px-3 py-2 text-sm">
                <TraceStatusPill status={t.status} />
                {t.name && <Badge variant="outline" className="text-[10px]">{t.name}</Badge>}
                {t.summary && <span className="min-w-0 break-words text-muted-foreground">{t.summary}</span>}
                {t.error && <span className="text-status-failed">{t.error}</span>}
                <span className="ml-auto font-mono text-xs text-muted-foreground">
                  {t.startSeq ? `#${t.startSeq}` : ''}{t.endSeq ? `–#${t.endSeq}` : ''}
                </span>
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Child tasks — linkable to their detail pages. */}
      {trace.childTasks.length > 0 && (
        <Section icon={ListTree} title="Child tasks" count={trace.childTasks.length}>
          <ul className="space-y-1.5">
            {trace.childTasks.map((c) => (
              <li key={c.name} className="flex flex-wrap items-center gap-2 rounded-md border bg-card px-3 py-2 text-sm">
                {c.status && <TraceStatusPill status={c.status} />}
                <Link
                  to="/tasks/$taskId"
                  params={{ taskId: c.name }}
                  className="font-mono text-xs text-primary hover:underline"
                >
                  {c.name}
                </Link>
                {c.agent && <Badge variant="outline" className="text-[10px]">{c.agent}</Badge>}
                {c.summary && <span className="min-w-0 break-words text-muted-foreground">{c.summary}</span>}
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Workspace */}
      {trace.workspace.length > 0 && (
        <Section icon={FolderGit2} title="Workspace" count={trace.workspace.length}>
          <ul className="space-y-1.5">
            {trace.workspace.map((w, i) => (
              <li key={`ws-${i}`} className="flex flex-wrap items-center gap-2 rounded-md border bg-card px-3 py-2 text-sm">
                <TraceStatusPill status={w.status} />
                {w.summary && <span className="min-w-0 break-words text-muted-foreground">{w.summary}</span>}
                <span className="ml-auto font-mono text-xs text-muted-foreground">#{w.seq}</span>
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Artifacts */}
      {trace.artifacts.length > 0 && (
        <Section icon={Package} title="Artifacts" count={trace.artifacts.length}>
          <ul className="space-y-1.5">
            {trace.artifacts.map((a, i) => (
              <li key={`art-${i}`} className="flex flex-wrap items-center gap-2 rounded-md border bg-card px-3 py-2 text-sm">
                <TraceStatusPill status={a.status} />
                {a.name && <span className="font-mono text-xs">{a.name}</span>}
                {a.summary && <span className="min-w-0 break-words text-muted-foreground">{a.summary}</span>}
                <span className="ml-auto font-mono text-xs text-muted-foreground">#{a.seq}</span>
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Approvals — derived from the timeline. */}
      {approvalEvents.length > 0 && (
        <Section icon={ShieldCheck} title="Approvals" count={approvalEvents.length}>
          <ul className="space-y-2">
            {approvalEvents.map((e) => (
              <li key={`ap-${e.seq}`}>
                <EventRow event={timelineToEvent(e, trace.task.namespace, trace.task.name)} />
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Fork provenance — derived from the timeline. */}
      {forkEvents.length > 0 && (
        <Section icon={GitFork} title="Fork provenance" count={forkEvents.length}>
          <ul className="space-y-2">
            {forkEvents.map((e) => (
              <li key={`fork-${e.seq}`}>
                <EventRow event={timelineToEvent(e, trace.task.namespace, trace.task.name)} />
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Warnings */}
      {trace.warnings.length > 0 && (
        <Section icon={TriangleAlert} title="Warnings" count={trace.warnings.length}>
          <ul className="space-y-1.5">
            {trace.warnings.map((issue, i) => (
              <li key={`warn-${i}`} className="flex items-start gap-2 rounded-md border border-status-pending/40 bg-status-pending-bg px-3 py-2 text-sm">
                <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0 text-status-pending" aria-hidden="true" />
                <span className="min-w-0 break-words">{issue.message}</span>
              </li>
            ))}
          </ul>
        </Section>
      )}

      {/* Raw timeline fallback — shown when the trace has no structured groups
          but events exist, so nothing is hidden. */}
      {!hasStructured && trace.timeline.length > 0 && (
        <Section icon={Activity} title="Raw timeline" count={trace.timeline.length}>
          <ul className="space-y-2">
            {trace.timeline.map((e) => (
              <li key={`tl-${e.seq}`}>
                <EventRow event={timelineToEvent(e, trace.task.namespace, trace.task.name)} />
              </li>
            ))}
          </ul>
        </Section>
      )}

      {!hasStructured && trace.timeline.length === 0 && (
        <p className="rounded-md border border-dashed px-4 py-6 text-center text-sm text-muted-foreground">
          No execution events recorded for this task yet.
        </p>
      )}
    </div>
  )
}

// Adapt a trace TraceEvent into the ExecutionEvent shape EventRow consumes. The
// trace event is a subset, so missing identity fields are filled from the task.
function timelineToEvent(e: TraceEvent, namespace: string, taskName: string) {
  return {
    id: `${taskName}:${e.seq}`,
    namespace,
    streamType: 'task',
    streamID: taskName,
    seq: e.seq,
    type: e.type,
    severity: e.severity,
    taskName: e.taskName || taskName,
    agentName: e.agentName,
    toolName: e.toolName,
    toolCallID: e.toolCallID,
    summary: summaryText(e),
    content: e.content,
    contentText: e.contentText,
    truncation: e.truncation,
    createdAt: e.createdAt,
  }
}
