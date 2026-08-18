import { z } from 'zod'
import { harnessContractVersionSchema } from './task'

const sessionExecutionLineageSchema = z.object({
  namespaceUID: z.string(),
  sessionUID: z.string(),
  contractVersion: harnessContractVersionSchema,
  generation: z.number(),
  runtimeIdentity: z.string(),
  configDigest: z.string(),
  establishedAt: z.string(),
})

const sessionExecutionControlSchema = z.object({
  resourceVersion: z.string().optional(),
  sessionUID: z.string(),
  runtimePoolRef: z.string().optional(),
  runtimeProfileDigest: z.string().optional(),
  generation: z.number().optional(),
  lifecycle: z.string().optional(),
  availability: z.enum(['Available', 'ReconciliationBlocked']).optional(),
  mutationLeaseGeneration: z.number().optional(),
  blockedReason: z.string().optional(),
  relatedPromptAttemptID: z.string().optional(),
  relatedPublicationID: z.string().optional(),
  lineage: sessionExecutionLineageSchema.optional(),
})

export const sessionSchema = z.object({
  name: z.string(),
  namespace: z.string(),
  transcript: z.string().optional(),
  messageCount: z.string().optional(),
  inputTokens: z.string().optional(),
  outputTokens: z.string().optional(),
  activeTask: z.string().optional(),
  executionControl: sessionExecutionControlSchema.optional(),
  createdAt: z.string().optional(),
  updatedAt: z.string().optional(),
})

export const sessionListItemSchema = z.object({
  name: z.string(),
  namespace: z.string(),
  messageCount: z.string().optional(),
  inputTokens: z.string().optional(),
  outputTokens: z.string().optional(),
  activeTask: z.string().optional(),
  createdAt: z.string().optional(),
  updatedAt: z.string().optional(),
})

export const transcriptMessageSchema = z.object({
  role: z.string(),
  content: z.string(),
  timestamp: z.string().optional(),
  model: z.string().optional(),
  inputTokens: z.number().optional(),
  outputTokens: z.number().optional(),
})

export type Session = z.infer<typeof sessionSchema>
export type SessionListItem = z.infer<typeof sessionListItemSchema>
export type TranscriptMessage = z.infer<typeof transcriptMessageSchema>
