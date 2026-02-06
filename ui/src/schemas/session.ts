import { z } from 'zod'

export const sessionSchema = z.object({
  name: z.string(),
  namespace: z.string(),
  transcript: z.string().optional(),
  messageCount: z.string().optional(),
  inputTokens: z.string().optional(),
  outputTokens: z.string().optional(),
  activeTask: z.string().optional(),
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
