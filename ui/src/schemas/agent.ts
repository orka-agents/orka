import { z } from 'zod'
import { conditionSchema } from './task'

export const modelConfigSchema = z.object({
  provider: z.string().optional(),
  name: z.string().optional(),
  temperature: z.number().optional(),
  maxTokens: z.number().optional(),
})

export const toolRefSchema = z.object({
  name: z.string(),
  enabled: z.boolean().optional(),
})

export const agentCLIRuntimeSchema = z.object({
  type: z.enum(['copilot', 'claude', 'codex', 'opencode']),
  defaultMaxTurns: z.number().optional(),
  defaultAllowedTools: z.array(z.string()).optional(),
  defaultAllowBash: z.boolean().optional(),
  defaultReasoningEffort: z.enum(['low', 'medium', 'high', 'xhigh', 'max']).optional(),
})

export const agentSpecSchema = z.object({
  providerRef: z.object({ name: z.string(), namespace: z.string().optional() }).optional(),
  model: modelConfigSchema.optional(),
  systemPrompt: z.object({
    inline: z.string().optional(),
    configMapRef: z.object({ name: z.string(), key: z.string() }).optional(),
  }).optional(),
  tools: z.array(toolRefSchema).optional(),
  skills: z.array(z.object({ configMapRef: z.object({ name: z.string(), key: z.string().optional() }) })).optional(),
  resources: z.any().optional(),
  secretRef: z.object({ name: z.string() }).optional(),
  session: z.object({
    persistence: z.string().optional(),
    ttl: z.string().optional(),
    maxMessages: z.number().optional(),
  }).optional(),
  rateLimit: z.object({
    requestsPerMinute: z.number().optional(),
    tokensPerMinute: z.number().optional(),
  }).optional(),
  coordination: z.object({
    enabled: z.boolean(),
    allowedAgents: z.array(z.object({ name: z.string(), namespace: z.string().optional() })).optional(),
    maxConcurrentChildren: z.number().optional(),
    maxDepth: z.number().optional(),
  }).optional(),
  runtime: agentCLIRuntimeSchema.optional(),
})

export const agentStatusSchema = z.object({
  activeTasks: z.number(),
  lastUsed: z.string().optional(),
  conditions: z.array(conditionSchema).optional(),
})

export const agentSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: z.object({
    name: z.string(),
    namespace: z.string().optional(),
    uid: z.string().optional(),
    creationTimestamp: z.string().optional(),
    labels: z.record(z.string()).optional(),
    annotations: z.record(z.string()).optional(),
  }),
  spec: agentSpecSchema,
  status: agentStatusSchema.optional(),
})

export type Agent = z.infer<typeof agentSchema>
export type AgentSpec = z.infer<typeof agentSpecSchema>
export type AgentStatus = z.infer<typeof agentStatusSchema>
