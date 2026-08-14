import { z } from 'zod'

export const memorySchema = z.object({
  id: z.string(),
  namespace: z.string(),
  sessionName: z.string().optional(),
  agentName: z.string().optional(),
  taskName: z.string().optional(),
  parentTask: z.string().optional(),
  source: z.string().optional(),
  sourceProposalId: z.string().optional(),
  content: z.string(),
  tags: z.array(z.string()).nullable().optional(),
  disabled: z.boolean().optional(),
  deleted: z.boolean().optional(),
  createdAt: z.string(),
  updatedAt: z.string(),
  lastRecalledAt: z.string().nullable().optional(),
  recalledCount: z.number().optional(),
})

export const memoryProposalStatusSchema = z.enum([
  'pending',
  'accepted',
  'rejected',
  'archived',
  'applied',
])

export const memoryProposalSchema = z.object({
  id: z.string(),
  namespace: z.string(),
  taskName: z.string().optional(),
  agentName: z.string().optional(),
  type: z.string().optional(),
  skillName: z.string().optional(),
  title: z.string(),
  description: z.string().optional(),
  content: z.string().optional(),
  patch: z.string().optional(),
  status: z.string(),
  reviewer: z.string().optional(),
  reviewNote: z.string().optional(),
  appliedMemoryId: z.string().optional(),
  appliedBy: z.string().optional(),
  createdAt: z.string(),
  updatedAt: z.string(),
  reviewedAt: z.string().nullable().optional(),
  appliedAt: z.string().nullable().optional(),
})

export interface MemoryFilter {
  query?: string
  sessionName?: string
  agentName?: string
  taskName?: string
  parentTask?: string
  source?: string
  tags?: string[]
  includeDisabled?: boolean
  includeDeleted?: boolean
  limit?: number
}

export interface MemoryProposalFilter {
  query?: string
  taskName?: string
  agentName?: string
  type?: string
  status?: string
  limit?: number
}

export type Memory = z.infer<typeof memorySchema>
export type MemoryProposal = z.infer<typeof memoryProposalSchema>
export type MemoryProposalStatus = z.infer<typeof memoryProposalStatusSchema>
