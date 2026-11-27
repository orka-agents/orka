import { z } from 'zod'

export const providerListItemSchema = z
  .object({
    name: z.string().optional(),
    namespace: z.string().optional(),
    type: z.string().optional(),
    defaultModel: z.string().optional(),
    ready: z.boolean().optional(),
    metadata: z.object({ name: z.string(), namespace: z.string().optional() }).optional(),
    spec: z.object({ type: z.string().optional(), defaultModel: z.string().optional() }).passthrough().optional(),
    status: z.object({ ready: z.boolean().optional() }).passthrough().optional(),
  })
  .passthrough()
  .transform((item) => ({
    name: item.name ?? item.metadata?.name ?? '',
    namespace: item.namespace ?? item.metadata?.namespace,
    type: item.type ?? item.spec?.type ?? '',
    defaultModel: item.defaultModel ?? item.spec?.defaultModel ?? '',
    ready: item.ready ?? (item.metadata ? item.status?.ready ?? false : undefined),
  }))
  .refine((item) => item.name.length > 0, { message: 'provider name is required' })

export const providerListResponseSchema = z.object({
  items: z.array(providerListItemSchema).default([]),
  metadata: z.object({ continue: z.string().optional(), remainingItemCount: z.number().optional() }).optional(),
})

export type ProviderListItem = z.infer<typeof providerListItemSchema>
