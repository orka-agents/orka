import { z } from 'zod'

export const taskTypeSchema = z.enum(['container', 'ai', 'agent'])
export const taskPhaseSchema = z.enum(['Pending', 'Running', 'Finalizing', 'Succeeded', 'Failed', 'Scheduled', 'Cancelled'])

export const conditionSchema = z.object({
  type: z.string(),
  status: z.string(),
  reason: z.string().optional(),
  message: z.string().optional(),
  lastTransitionTime: z.string().optional(),
})

export const retryPolicySchema = z.object({
  maxRetries: z.number().optional(),
  backoffMultiplier: z.number().optional(),
  initialDelay: z.string().optional(),
})

export const secretRefSchema = z.object({
  name: z.string(),
  namespace: z.string().optional(),
})

export const sessionRefSchema = z.object({
  name: z.string(),
  create: z.boolean().optional(),
  append: z.boolean().optional(),
  maxMessages: z.number().optional(),
})

export const agentRefSchema = z.object({
  name: z.string(),
  namespace: z.string().optional(),
})

export const aiSpecSchema = z.object({
  providerRef: z.object({ name: z.string(), namespace: z.string().optional() }).optional(),
  provider: z.string().optional(),
  model: z.string().optional(),
  prompt: z.string().optional(),
  systemPrompt: z.string().optional(),
  temperature: z.number().optional(),
  maxTokens: z.number().optional(),
  skills: z.array(z.object({ configMapRef: z.object({ name: z.string(), key: z.string().optional() }) })).optional(),
  tools: z.array(z.string()).optional(),
})

export const workspaceConfigSchema = z.object({
  gitRepo: z.string().optional(),
  branch: z.string().optional(),
  ref: z.string().optional(),
  pushBranch: z.string().optional(),
  gitSecretRef: z.object({ name: z.string() }).optional(),
  subPath: z.string().optional(),
})

export const agentRuntimeSpecSchema = z.object({
  workspace: workspaceConfigSchema.optional(),
  maxTurns: z.number().optional(),
  allowedTools: z.array(z.string()).optional(),
  disallowedTools: z.array(z.string()).optional(),
  allowBash: z.boolean().optional(),
})

export const resultRefSchema = z.object({
  // Backend ResultReference serializes as { available: bool }; older callers
  // referenced ConfigMap fields, so keep them optional for tolerance.
  available: z.boolean().optional(),
  configMapName: z.string().optional(),
  key: z.string().optional(),
})

export const childTaskStatusSchema = z.object({
  name: z.string(),
  agent: z.string(),
  phase: taskPhaseSchema,
  result: z.string().optional(),
})

// Mirrors the safe, non-secret surface of api/v1alpha1 ExecutionWorkspaceStatus.
// Provider credentials and unsafe identifiers are deliberately excluded — only
// provider-neutral lifecycle/placement/density metadata is parsed for UI.
export const executionWorkspacePlacementSchema = z.object({
  workerNamespace: z.string().optional(),
  workerPool: z.string().optional(),
  workerPodName: z.string().optional(),
})

export const executionWorkspaceDensitySchema = z.object({
  workerCount: z.number().optional(),
  actorCount: z.number().optional(),
  runningActorCount: z.number().optional(),
  suspendedActorCount: z.number().optional(),
  actorsPerWorker: z.string().optional(),
})

export const executionWorkspaceStatusSchema = z.object({
  provider: z.string().optional(),
  templateRef: z.object({ name: z.string().optional() }).optional(),
  phase: z.string().optional(),
  reason: z.string().optional(),
  reusePolicy: z.string().optional(),
  cleanupPolicy: z.string().optional(),
  reused: z.boolean().optional(),
  placement: executionWorkspacePlacementSchema.optional(),
  density: executionWorkspaceDensitySchema.optional(),
  resumeLatency: z.string().optional(),
  message: z.string().optional(),
  lastUpdateTime: z.string().optional(),
})

export const taskSpecSchema = z.object({
  type: taskTypeSchema,
  image: z.string().optional(),
  command: z.array(z.string()).optional(),
  args: z.array(z.string()).optional(),
  env: z.array(z.object({ name: z.string(), value: z.string().optional() })).optional(),
  timeout: z.string().optional(),
  priority: z.number().optional(),
  retryPolicy: retryPolicySchema.optional(),
  webhookURL: z.string().optional(),
  secretRef: secretRefSchema.optional(),
  sessionRef: sessionRefSchema.optional(),
  resources: z.any().optional(),
  ai: aiSpecSchema.optional(),
  agentRef: agentRefSchema.optional(),
  prompt: z.string().optional(),
  agentRuntime: agentRuntimeSpecSchema.optional(),
})

export const taskStatusSchema = z.object({
  phase: taskPhaseSchema.optional(),
  startTime: z.string().optional(),
  completionTime: z.string().optional(),
  attempts: z.number().optional(),
  iteration: z.number().optional(),
  jobName: z.string().optional(),
  resultRef: resultRefSchema.optional(),
  webhookDelivered: z.boolean().optional(),
  message: z.string().optional(),
  childTasks: z.array(childTaskStatusSchema).optional(),
  conditions: z.array(conditionSchema).optional(),
  executionWorkspace: executionWorkspaceStatusSchema.optional(),
})

export const k8sMetadataSchema = z.object({
  name: z.string(),
  namespace: z.string().optional(),
  uid: z.string().optional(),
  creationTimestamp: z.string().optional(),
  labels: z.record(z.string()).optional(),
  annotations: z.record(z.string()).optional(),
})

export const taskSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: k8sMetadataSchema,
  spec: taskSpecSchema,
  status: taskStatusSchema.optional(),
})

export type Task = z.infer<typeof taskSchema>
export type TaskSpec = z.infer<typeof taskSpecSchema>
export type TaskStatus = z.infer<typeof taskStatusSchema>
export type TaskType = z.infer<typeof taskTypeSchema>
export type TaskPhase = z.infer<typeof taskPhaseSchema>
export type ExecutionWorkspaceStatus = z.infer<typeof executionWorkspaceStatusSchema>

export const planStateSchema = z.object({
  summary: z.string().optional(),
  progressPct: z.number().optional(),
  goalComplete: z.boolean().optional(),
  planDocument: z.string().optional(),
  iteration: z.number().optional(),
})

export const taskWithPlanSchema = taskSchema.extend({
  plan: planStateSchema.optional(),
})

export type PlanState = z.infer<typeof planStateSchema>
export type TaskWithPlan = z.infer<typeof taskWithPlanSchema>

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
  provider: z.string().optional(),
  model: z.string().optional(),
  stopReason: z.string().optional(),
  inputTokens: z.number().optional(),
  outputTokens: z.number().optional(),
  summary: z.string().optional(),
  content: z.unknown().optional(),
  contentText: z.string().optional(),
  createdAt: z.string(),
})

export const taskEventsResponseSchema = z.object({
  namespace: z.string(),
  streamType: z.string(),
  streamID: z.string(),
  afterSeq: z.number(),
  latestSeq: z.number(),
  events: z.array(executionEventSchema),
})

export type ExecutionEvent = z.infer<typeof executionEventSchema>
export type TaskEventsResponse = z.infer<typeof taskEventsResponseSchema>
