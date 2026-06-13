import { z } from 'zod'

export const taskTypeSchema = z.enum(['container', 'ai', 'agent'])
export const taskPhaseSchema = z.enum(['Pending', 'Running', 'Succeeded', 'Failed', 'Scheduled', 'Cancelled'])

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
  configMapName: z.string(),
  key: z.string().optional(),
})

export const childTaskStatusSchema = z.object({
  name: z.string(),
  agent: z.string(),
  phase: taskPhaseSchema,
  result: z.string().optional(),
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
