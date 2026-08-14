import { z } from 'zod'
import { conditionSchema, k8sMetadataSchema } from './task'

export const providerTypeSchema = z.enum(['anthropic', 'openai', 'azure-openai'])

export const providerRateLimitSchema = z.object({
  requestsPerMinute: z.number().optional(),
  tokensPerMinute: z.number().optional(),
})

export const providerSpecSchema = z.object({
  type: providerTypeSchema,
  secretRef: z.object({
    name: z.string(),
    key: z.string().optional(),
  }),
  baseURL: z.string().optional(),
  defaultModel: z.string().optional(),
  rateLimit: providerRateLimitSchema.optional(),
  azure: z.object({
    deploymentName: z.string().optional(),
    apiVersion: z.string().optional(),
  }).optional(),
})

export const providerStatusSchema = z.object({
  ready: z.boolean().optional(),
  lastValidated: z.string().optional(),
  message: z.string().optional(),
  conditions: z.array(conditionSchema).optional(),
})

export const providerSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: k8sMetadataSchema,
  spec: providerSpecSchema,
  status: providerStatusSchema.optional(),
})

// GET /providers returns flat read items, not full Provider objects.
export const providerListItemSchema = z.object({
  name: z.string(),
  namespace: z.string().optional(),
  type: z.string().optional(),
  defaultModel: z.string().optional(),
  ready: z.boolean().optional(),
})

export type Provider = z.infer<typeof providerSchema>
export type ProviderSpec = z.infer<typeof providerSpecSchema>
export type ProviderType = z.infer<typeof providerTypeSchema>
export type ProviderListItem = z.infer<typeof providerListItemSchema>
