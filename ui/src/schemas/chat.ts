import { z } from 'zod'

export const chatRequestSchema = z.object({
  message: z.string(),
  sessionId: z.string().optional(),
  namespace: z.string().optional(),
  provider: z.string().optional(),
  model: z.string().optional(),
  temperature: z.number().optional(),
  maxTokens: z.number().optional(),
  systemPrompt: z.string().optional(),
  agentRef: z.string().optional(),
})

export type ChatRequest = z.infer<typeof chatRequestSchema>

export const chatUsageSchema = z.object({
  inputTokens: z.number().optional(),
  outputTokens: z.number().optional(),
  llmCalls: z.number().optional(),
  toolCalls: z.number().optional(),
  tasksCreated: z.number().optional(),
  duration: z.string().optional(),
})

export type ChatUsage = z.infer<typeof chatUsageSchema>

export const chatToolCallSchema = z.object({
  name: z.string(),
  args: z.unknown(),
  result: z.unknown().optional(),
})

export type ChatToolCall = z.infer<typeof chatToolCallSchema>

export const chatResponseSchema = z.object({
  sessionId: z.string(),
  message: z.string(),
  toolCalls: z.array(chatToolCallSchema).optional(),
  usage: chatUsageSchema.optional(),
})

export type ChatResponse = z.infer<typeof chatResponseSchema>

export const chatConfigSchema = z.object({
  enabled: z.boolean(),
  provider: z.string(),
  model: z.string(),
  maxIterations: z.number(),
  maxDuration: z.string(),
  maxTasksPerTurn: z.number(),
  maxConcurrent: z.number(),
  availableTools: z.array(z.string()),
  requireExplicitProvider: z.boolean().optional(),
  connectorsEnabled: z.boolean().optional(),
})

export type ChatConfig = z.infer<typeof chatConfigSchema>

export interface SSEStatusEvent {
  sessionId: string
  provider: string
  model: string
}

export interface SSEToolCallEvent {
  id: string
  name: string
  args: unknown
}

export interface SSEToolResultEvent {
  id: string
  name: string
  result: unknown
}

export interface SSEMessageEvent {
  content: string
}

export interface SSEDoneEvent {
  usage: ChatUsage
}

type ChatMessageRole = 'user' | 'assistant' | 'tool_call' | 'tool_result' | 'status' | 'error'

export interface ChatMessage {
  id: string
  role: ChatMessageRole
  content: string
  timestamp: string
  toolCallId?: string
  toolName?: string
  toolArgs?: unknown
  toolResult?: unknown
  toolSuccess?: boolean
  usage?: ChatUsage
  tasksCreatedNames?: string[]
  provider?: string
  model?: string
  sessionId?: string
}
