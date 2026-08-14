import { z } from 'zod'
import { conditionSchema, k8sMetadataSchema } from './task'

export const skillContentSchema = z.object({
  inline: z.string(),
  files: z.record(z.string()).optional(),
})

export const skillSourceSchema = z.object({
  github: z.string().optional(),
  skillName: z.string().optional(),
  context7: z.boolean().optional(),
})

export const skillSpecSchema = z.object({
  displayName: z.string().optional(),
  description: z.string(),
  version: z.string().optional(),
  author: z.string().optional(),
  tags: z.array(z.string()).optional(),
  content: skillContentSchema,
  source: skillSourceSchema.optional(),
})

export const skillStatusSchema = z.object({
  phase: z.string().optional(),
  contentHash: z.string().optional(),
  observedGeneration: z.number().optional(),
  conditions: z.array(conditionSchema).optional(),
})

export const skillSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: k8sMetadataSchema,
  spec: skillSpecSchema,
  status: skillStatusSchema.optional(),
})

// GET /skills returns flat read items, not full Skill objects.
export const skillListItemSchema = z.object({
  name: z.string(),
  namespace: z.string().optional(),
  displayName: z.string().optional(),
  description: z.string().optional(),
  version: z.string().optional(),
  author: z.string().optional(),
  tags: z.array(z.string()).nullable().optional(),
  phase: z.string().optional(),
})

export type Skill = z.infer<typeof skillSchema>
export type SkillSpec = z.infer<typeof skillSpecSchema>
export type SkillListItem = z.infer<typeof skillListItemSchema>
