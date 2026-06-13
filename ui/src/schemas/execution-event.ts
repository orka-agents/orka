import { z } from 'zod'

// Mirrors internal/events ExecutionEventSeverity* constants.
export const executionEventSeveritySchema = z.enum(['debug', 'info', 'warning', 'error'])

// Mirrors internal/events/redaction.go ExecutionEventTruncation.
export const executionEventTruncationSchema = z.object({
  summaryTruncated: z.boolean().optional(),
  summaryOriginalChars: z.number().optional(),
  contentTextTruncated: z.boolean().optional(),
  contentTextOriginalChars: z.number().optional(),
  contentJsonTruncated: z.boolean().optional(),
  contentJsonOriginalBytes: z.number().optional(),
})

// Mirrors internal/api/execution_event_dto.go ExecutionEventResponse, plus the
// optional taskSeq/taskStreamID fields that only appear on aggregated session
// streams (SessionExecutionEventResponse). Keeping them optional lets one type
// describe both task and session event frames.
export const executionEventSchema = z.object({
  id: z.string(),
  namespace: z.string(),
  streamType: z.string(),
  streamID: z.string(),
  seq: z.number(),
  type: z.string(),
  severity: z.string(),
  taskName: z.string().optional(),
  sessionName: z.string().optional(),
  agentName: z.string().optional(),
  toolName: z.string().optional(),
  toolCallID: z.string().optional(),
  summary: z.string().optional(),
  content: z.unknown().optional(),
  contentText: z.string().optional(),
  truncation: executionEventTruncationSchema.optional(),
  createdAt: z.string(),
  // Session-stream only.
  taskSeq: z.number().optional(),
  taskStreamID: z.string().optional(),
})

export const listExecutionEventsResponseSchema = z.object({
  namespace: z.string(),
  streamType: z.string(),
  streamID: z.string(),
  afterSeq: z.number(),
  latestSeq: z.number(),
  events: z.array(executionEventSchema),
})

// stream_complete SSE frame payload.
export const streamCompleteSchema = z.object({
  lastSeq: z.number(),
  type: z.string(),
})

export type ExecutionEventSeverity = z.infer<typeof executionEventSeveritySchema>
export type ExecutionEventTruncation = z.infer<typeof executionEventTruncationSchema>
export type ExecutionEvent = z.infer<typeof executionEventSchema>
export type ListExecutionEventsResponse = z.infer<typeof listExecutionEventsResponseSchema>
export type StreamComplete = z.infer<typeof streamCompleteSchema>

// ---- Task trace (mirrors internal/tasktrace/tasktrace.go) ----

export const traceEventSchema = z.object({
  seq: z.number(),
  type: z.string(),
  severity: z.string(),
  summary: z.string().optional(),
  taskName: z.string().optional(),
  agentName: z.string().optional(),
  toolName: z.string().optional(),
  toolCallID: z.string().optional(),
  content: z.unknown().optional(),
  contentText: z.string().optional(),
  truncation: executionEventTruncationSchema.optional(),
  createdAt: z.string(),
})

export const modelRequestTraceSchema = z.object({
  id: z.string(),
  status: z.string(),
  startSeq: z.number().optional(),
  endSeq: z.number().optional(),
  startedAt: z.string().optional(),
  endedAt: z.string().optional(),
  summary: z.string().optional(),
  error: z.string().optional(),
})

export const toolCallTraceSchema = z.object({
  id: z.string(),
  name: z.string().optional(),
  status: z.string(),
  startSeq: z.number().optional(),
  endSeq: z.number().optional(),
  startedAt: z.string().optional(),
  endedAt: z.string().optional(),
  summary: z.string().optional(),
  error: z.string().optional(),
})

export const childTaskTraceSchema = z.object({
  name: z.string(),
  agent: z.string().optional(),
  status: z.string().optional(),
  startSeq: z.number().optional(),
  endSeq: z.number().optional(),
  startedAt: z.string().optional(),
  endedAt: z.string().optional(),
  summary: z.string().optional(),
  result: z.string().optional(),
})

export const workspaceTraceSchema = z.object({
  status: z.string(),
  seq: z.number(),
  summary: z.string().optional(),
  createdAt: z.string(),
})

export const artifactTraceSchema = z.object({
  name: z.string().optional(),
  status: z.string(),
  seq: z.number(),
  summary: z.string().optional(),
  createdAt: z.string(),
})

export const traceIssueSchema = z.object({
  seq: z.number().optional(),
  type: z.string().optional(),
  severity: z.string().optional(),
  message: z.string(),
})

export const taskTraceSummarySchema = z.object({
  namespace: z.string(),
  name: z.string(),
  type: z.string().optional(),
  phase: z.string().optional(),
  agentName: z.string().optional(),
  sessionName: z.string().optional(),
  resultAvailable: z.boolean(),
})

export const taskTraceSchema = z.object({
  task: taskTraceSummarySchema,
  latestSeq: z.number(),
  generatedAt: z.string(),
  timeline: z.array(traceEventSchema),
  modelRequests: z.array(modelRequestTraceSchema),
  toolCalls: z.array(toolCallTraceSchema),
  childTasks: z.array(childTaskTraceSchema),
  workspace: z.array(workspaceTraceSchema),
  artifacts: z.array(artifactTraceSchema),
  errors: z.array(traceIssueSchema),
  warnings: z.array(traceIssueSchema),
  terminalEvent: traceEventSchema.optional(),
  rawUnpaired: z.array(traceEventSchema).optional(),
})

export type TraceEvent = z.infer<typeof traceEventSchema>
export type ModelRequestTrace = z.infer<typeof modelRequestTraceSchema>
export type ToolCallTrace = z.infer<typeof toolCallTraceSchema>
export type ChildTaskTrace = z.infer<typeof childTaskTraceSchema>
export type WorkspaceTrace = z.infer<typeof workspaceTraceSchema>
export type ArtifactTrace = z.infer<typeof artifactTraceSchema>
export type TraceIssue = z.infer<typeof traceIssueSchema>
export type TaskTraceSummary = z.infer<typeof taskTraceSummarySchema>
export type TaskTrace = z.infer<typeof taskTraceSchema>

// ---- Approvals (mirrors internal/approvals/approvals.go) ----

export const approvalStatusSchema = z.enum([
  'pending',
  'approved',
  'declined',
  'expired',
  'cancelled',
])

export const approvalSchema = z.object({
  id: z.string(),
  action: z.string(),
  riskSummary: z.string().optional(),
  toolCallID: z.string().optional(),
  status: z.string(),
  createdAt: z.string(),
  expiresAt: z.string().optional(),
  timeout: z.string().optional(),
  decisionSeq: z.number().optional(),
  decisionTime: z.string().optional(),
  decisionReason: z.string().optional(),
  decisionActor: z.string().optional(),
})

export const listTaskApprovalsResponseSchema = z.object({
  namespace: z.string(),
  taskName: z.string(),
  approvals: z.array(approvalSchema),
})

export type ApprovalStatus = z.infer<typeof approvalStatusSchema>
export type Approval = z.infer<typeof approvalSchema>
export type ListTaskApprovalsResponse = z.infer<typeof listTaskApprovalsResponseSchema>

// ---- Fork (mirrors internal/api/fork_handlers.go + internal/fork/context.go) ----

export const forkEventSummarySchema = z.object({
  seq: z.number(),
  type: z.string(),
  severity: z.string(),
  summary: z.string().optional(),
  toolName: z.string().optional(),
  toolCallID: z.string().optional(),
  content: z.unknown().optional(),
  contentText: z.string().optional(),
})

export const forkContextSchema = z.object({
  sourceNamespace: z.string(),
  sourceTask: z.string(),
  afterSeq: z.number(),
  events: z.array(forkEventSummarySchema),
  truncated: z.boolean(),
})

export const forkTaskResponseSchema = z.object({
  namespace: z.string(),
  sourceTaskName: z.string(),
  newTaskName: z.string(),
  afterSeq: z.number(),
  forkContext: forkContextSchema,
})

export interface ForkTaskRequest {
  afterSeq?: number
  newTaskName?: string
  agentRef?: { name: string; namespace?: string }
  prompt?: string
  workspace?: Record<string, unknown>
}

export type ForkEventSummary = z.infer<typeof forkEventSummarySchema>
export type ForkContext = z.infer<typeof forkContextSchema>
export type ForkTaskResponse = z.infer<typeof forkTaskResponseSchema>
