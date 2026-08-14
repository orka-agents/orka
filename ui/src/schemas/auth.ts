import { z } from 'zod'

export const whoAmITransactionSchema = z.object({
  profile: z.string().optional(),
  type: z.string().optional(),
  id: z.string().optional(),
  issuer: z.string().optional(),
  subject: z.string().optional(),
  audience: z.union([z.string(), z.array(z.string())]).optional(),
  scope: z.string().optional(),
  scopes: z.array(z.string()).optional(),
  requestingWorkload: z.string().optional(),
})

export const whoAmISchema = z.object({
  authenticated: z.boolean(),
  authType: z.string().optional(),
  username: z.string().optional(),
  uid: z.string().optional(),
  groups: z.array(z.string()).nullable().optional(),
  namespace: z.string().optional(),
  subject: z.string().optional(),
  email: z.string().optional(),
  issuer: z.string().optional(),
  roles: z.array(z.string()).nullable().optional(),
  transaction: whoAmITransactionSchema.nullable().optional(),
})

export type WhoAmI = z.infer<typeof whoAmISchema>
export type WhoAmITransaction = z.infer<typeof whoAmITransactionSchema>
