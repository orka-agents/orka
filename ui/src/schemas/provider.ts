import { z } from 'zod'

/**
 * GET /api/v1/providers returns full Provider CRDs for ServiceAccount/OIDC
 * callers and a flat {name, namespace, type, defaultModel, ready} projection
 * for transaction-token callers. Normalize both into one list item.
 */
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
    // Full CRDs serialize status.ready with omitempty, so a missing value on a
    // CRD-shaped item means false; only the flat projection may leave it unknown.
    ready: item.ready ?? (item.metadata ? item.status?.ready ?? false : undefined),
  }))
  .refine((item) => item.name.length > 0, { message: 'provider name is required' })

export const providerListResponseSchema = z.object({
  items: z.array(providerListItemSchema).default([]),
  metadata: z.object({ continue: z.string().optional(), remainingItemCount: z.number().optional() }).optional(),
})

export type ProviderListItem = z.infer<typeof providerListItemSchema>
