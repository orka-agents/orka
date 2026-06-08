import { z } from 'zod'
import { conditionSchema } from './task'

export const httpExecutionSchema = z.object({
  url: z.string().optional(),
  method: z.string().optional(),
  headers: z.record(z.string()).optional(),
  timeout: z.string().optional(),
  authSecretRef: z.object({ name: z.string(), key: z.string() }).optional(),
  authInject: z.string().optional(),
  authBodyKey: z.string().optional(),
})

export const workspaceTemplateReferenceSchema = z.object({
  name: z.string().optional(),
  namespace: z.string().optional(),
})

export const substrateActorPoolReferenceSchema = z.object({
  name: z.string().optional(),
  namespace: z.string().optional(),
})

export const mcpToolServerSchema = z.object({
  path: z.string().optional(),
  substrateActor: z.object({
    templateRef: workspaceTemplateReferenceSchema,
    poolRef: substrateActorPoolReferenceSchema.optional(),
    boot: z.boolean().optional(),
  }),
})

export const toolSpecSchema = z.object({
  description: z.string(),
  parameters: z.any().optional(),
  http: httpExecutionSchema.optional(),
  mcp: mcpToolServerSchema.optional(),
}).refine((spec) => spec.http || spec.mcp?.substrateActor, {
  message: 'http or mcp.substrateActor is required',
  path: ['http'],
}).refine((spec) => spec.mcp?.substrateActor || !spec.http || Boolean(spec.http.url), {
  message: 'http.url is required unless mcp.substrateActor is set',
  path: ['http', 'url'],
})

export const toolStatusSchema = z.object({
  available: z.boolean(),
  lastCheck: z.string().optional(),
  error: z.string().optional(),
  endpoint: z.string().optional(),
  actor: z.object({
    provider: z.string().optional(),
    actorID: z.string().optional(),
    routeHost: z.string().optional(),
    templateRef: workspaceTemplateReferenceSchema.optional(),
    poolRef: substrateActorPoolReferenceSchema.optional(),
  }).optional(),
  conditions: z.array(conditionSchema).optional(),
})

export const toolSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: z.object({
    name: z.string(),
    namespace: z.string().optional(),
    uid: z.string().optional(),
    creationTimestamp: z.string().optional(),
  }),
  spec: toolSpecSchema,
  status: toolStatusSchema.optional(),
})

// For list endpoint which returns a simplified format
export const toolListItemSchema = z.object({
  name: z.string(),
  namespace: z.string().optional(),
  builtin: z.boolean(),
  description: z.string(),
  available: z.boolean().optional(),
  url: z.string().optional(),
})

export type Tool = z.infer<typeof toolSchema>
export type ToolListItem = z.infer<typeof toolListItemSchema>
