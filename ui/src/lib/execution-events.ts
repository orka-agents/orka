import { API_BASE_URL } from './constants'
import {
  executionEventSchema,
  streamCompleteSchema,
  type ExecutionEvent,
  type StreamComplete,
} from '@/schemas/execution-event'

// Centralized URL builders for the execution-event surface so no component or
// hook hardcodes backend path strings.
export const executionEventApi = {
  taskEvents: (taskId: string) => `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/events`,
  taskStream: (taskId: string) => `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/stream`,
  taskTrace: (taskId: string) => `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/trace`,
  taskApprovals: (taskId: string) => `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/approvals`,
  taskApprovalDecision: (taskId: string, approvalId: string) =>
    `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/approvals/${encodeURIComponent(approvalId)}/decision`,
  taskFork: (taskId: string) => `${API_BASE_URL}/tasks/${encodeURIComponent(taskId)}/fork`,
  sessionEvents: (sessionId: string) =>
    `${API_BASE_URL}/sessions/${encodeURIComponent(sessionId)}/events`,
  sessionStream: (sessionId: string) =>
    `${API_BASE_URL}/sessions/${encodeURIComponent(sessionId)}/stream`,
} as const

// Relative (non base-prefixed) versions used by the shared `api` client, which
// prepends API_BASE_URL itself.
export const executionEventApiPath = {
  taskEvents: (taskId: string) => `/tasks/${encodeURIComponent(taskId)}/events`,
  taskTrace: (taskId: string) => `/tasks/${encodeURIComponent(taskId)}/trace`,
  taskApprovals: (taskId: string) => `/tasks/${encodeURIComponent(taskId)}/approvals`,
  taskApprovalDecision: (taskId: string, approvalId: string) =>
    `/tasks/${encodeURIComponent(taskId)}/approvals/${encodeURIComponent(approvalId)}/decision`,
  taskFork: (taskId: string) => `/tasks/${encodeURIComponent(taskId)}/fork`,
  sessionEvents: (sessionId: string) => `/sessions/${encodeURIComponent(sessionId)}/events`,
} as const

// Functional event categories, derived from the Wave 0 taxonomy in
// internal/events/execution_event.go. Error highlighting is driven by severity,
// not by category, so failures stay grouped with their functional area.
export type ExecutionEventCategory =
  | 'lifecycle'
  | 'worker'
  | 'model'
  | 'tools'
  | 'workspace'
  | 'artifacts'
  | 'approvals'
  | 'fork'
  | 'other'

const CATEGORY_BY_TYPE: Record<string, ExecutionEventCategory> = {
  TaskCreated: 'lifecycle',
  TaskPhaseChanged: 'lifecycle',
  TaskJobCreated: 'lifecycle',
  TaskStarted: 'lifecycle',
  TaskSucceeded: 'lifecycle',
  TaskFailed: 'lifecycle',
  TaskCancelled: 'lifecycle',
  TaskDeleted: 'lifecycle',
  WorkerStarted: 'worker',
  WorkerCompleted: 'worker',
  WorkerFailed: 'worker',
  AgentRuntimeStarted: 'worker',
  AgentRuntimeCommandStarted: 'worker',
  AgentRuntimeCompleted: 'worker',
  AgentRuntimeFailed: 'worker',
  AgentRuntimeCancelled: 'worker',
  ModelRequestStarted: 'model',
  ModelRequestCompleted: 'model',
  ModelRequestFailed: 'model',
  ModelMessage: 'model',
  ContextTruncated: 'model',
  ToolCallStarted: 'tools',
  ToolCallCompleted: 'tools',
  ToolCallFailed: 'tools',
  ToolCallSkipped: 'tools',
  WorkspacePreparationStarted: 'workspace',
  WorkspacePreparationCompleted: 'workspace',
  WorkspacePreparationFailed: 'workspace',
  ResultSubmitted: 'lifecycle',
  ArtifactUploadCompleted: 'artifacts',
  ArtifactUploadFailed: 'artifacts',
  TaskForkRequested: 'fork',
  TaskForkCreated: 'fork',
  ApprovalRequested: 'approvals',
  ApprovalApproved: 'approvals',
  ApprovalDeclined: 'approvals',
  ApprovalExpired: 'approvals',
  ApprovalCancelled: 'approvals',
}

export const EXECUTION_EVENT_CATEGORY_LABELS: Record<ExecutionEventCategory, string> = {
  lifecycle: 'Lifecycle',
  worker: 'Worker / Runtime',
  model: 'Model',
  tools: 'Tools',
  workspace: 'Workspace',
  artifacts: 'Artifacts',
  approvals: 'Approvals',
  fork: 'Fork',
  other: 'Other',
}

// Stable category ordering for grouped display.
export const EXECUTION_EVENT_CATEGORY_ORDER: ExecutionEventCategory[] = [
  'lifecycle',
  'worker',
  'model',
  'tools',
  'workspace',
  'artifacts',
  'approvals',
  'fork',
  'other',
]

export function executionEventCategory(type: string): ExecutionEventCategory {
  return CATEGORY_BY_TYPE[type] ?? 'other'
}

export type ExecutionEventSeverityLevel = 'debug' | 'info' | 'warning' | 'error'

// Coerce arbitrary severity strings to a known level (API normalizes to info).
export function normalizeSeverity(severity?: string): ExecutionEventSeverityLevel {
  switch ((severity ?? '').toLowerCase()) {
    case 'debug':
      return 'debug'
    case 'warning':
      return 'warning'
    case 'error':
      return 'error'
    default:
      return 'info'
  }
}

export function isTruncated(event: { truncation?: Record<string, unknown> | null }): boolean {
  const t = event.truncation
  if (!t) return false
  return Boolean(
    t.summaryTruncated || t.contentTextTruncated || t.contentJsonTruncated,
  )
}

// Merge two event lists deduped by seq and sorted ascending. Later sources win
// on seq collisions, which lets live stream events supersede the initial replay.
export function mergeEventsBySeq<T extends { seq: number }>(...sources: T[][]): T[] {
  const bySeq = new Map<number, T>()
  for (const source of sources) {
    for (const event of source) {
      bySeq.set(event.seq, event)
    }
  }
  return Array.from(bySeq.values()).sort((a, b) => a.seq - b.seq)
}

// Highest seq across a list, or fallback when empty.
export function maxSeq(events: { seq: number }[], fallback = 0): number {
  let m = fallback
  for (const e of events) if (e.seq > m) m = e.seq
  return m
}

// ---- SSE frame parsing ----

// A single parsed SSE frame, classified by its `event:` field. The backend emits
// `execution_event`, `stream_complete`, and bare `: heartbeat` comment frames.
export type ExecutionEventFrame =
  | { kind: 'event'; seq: number; event: ExecutionEvent }
  | { kind: 'complete'; seq: number; complete: StreamComplete }
  | { kind: 'heartbeat' }
  | { kind: 'unknown' }

interface RawSSEFrame {
  id?: string
  event?: string
  data: string
  isComment: boolean
}

// Parse one SSE block (the text between blank-line separators) into raw fields.
// Returns null only for an entirely empty block. Comment-only blocks (lines
// starting with ':') are returned with isComment=true.
export function parseSSEBlock(block: string): RawSSEFrame | null {
  const lines = block.split('\n')
  let id: string | undefined
  let event: string | undefined
  const dataLines: string[] = []
  let sawContent = false
  let sawComment = false

  for (const rawLine of lines) {
    const line = rawLine.replace(/\r$/, '')
    if (line === '') continue
    if (line.startsWith(':')) {
      sawComment = true
      continue
    }
    sawContent = true
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    // Per the SSE spec, a single space after the colon is stripped.
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'id':
        id = value
        break
      case 'event':
        event = value
        break
      case 'data':
        dataLines.push(value)
        break
      default:
        break
    }
  }

  if (!sawContent) {
    return sawComment ? { data: '', isComment: true } : null
  }
  return { id, event, data: dataLines.join('\n'), isComment: false }
}

// Parse and classify one SSE block into a typed execution-event frame. Invalid
// JSON or schema-mismatched payloads classify as 'unknown' so the stream loop can
// skip them without advancing its cursor.
export function parseExecutionEventFrame(block: string): ExecutionEventFrame | null {
  const raw = parseSSEBlock(block)
  if (raw === null) return null
  if (raw.isComment) return { kind: 'heartbeat' }

  if (raw.event === 'stream_complete') {
    try {
      const parsed = streamCompleteSchema.safeParse(JSON.parse(raw.data))
      if (parsed.success) {
        return { kind: 'complete', seq: parsed.data.lastSeq, complete: parsed.data }
      }
    } catch {
      // fall through to unknown
    }
    return { kind: 'unknown' }
  }

  if (raw.event === 'execution_event' || raw.event === undefined) {
    try {
      const parsed = executionEventSchema.safeParse(JSON.parse(raw.data))
      if (parsed.success) {
        return { kind: 'event', seq: parsed.data.seq, event: parsed.data }
      }
    } catch {
      // fall through to unknown
    }
    return { kind: 'unknown' }
  }

  return { kind: 'unknown' }
}

// Incremental SSE buffer: feed decoded chunks, yields complete typed frames as
// blank-line-delimited blocks become available. Used by the streaming hook.
export class SSEFrameBuffer {
  private buffer = ''

  push(chunk: string): ExecutionEventFrame[] {
    this.buffer += chunk
    const frames: ExecutionEventFrame[] = []
    let sepIndex = this.indexOfSeparator()
    while (sepIndex.index !== -1) {
      const block = this.buffer.slice(0, sepIndex.index)
      this.buffer = this.buffer.slice(sepIndex.index + sepIndex.length)
      const frame = parseExecutionEventFrame(block)
      if (frame) frames.push(frame)
      sepIndex = this.indexOfSeparator()
    }
    return frames
  }

  // Flush any trailing block (stream closed without a final blank line).
  flush(): ExecutionEventFrame[] {
    const remaining = this.buffer.trim()
    this.buffer = ''
    if (!remaining) return []
    const frame = parseExecutionEventFrame(remaining)
    return frame ? [frame] : []
  }

  private indexOfSeparator(): { index: number; length: number } {
    const lf = this.buffer.indexOf('\n\n')
    const crlf = this.buffer.indexOf('\r\n\r\n')
    if (lf === -1 && crlf === -1) return { index: -1, length: 0 }
    if (crlf === -1) return { index: lf, length: 2 }
    if (lf === -1) return { index: crlf, length: 4 }
    return lf < crlf ? { index: lf, length: 2 } : { index: crlf, length: 4 }
  }
}
