import { z } from 'zod'

export const artifactMetadataSchema = z.object({
  filename: z.string(),
  contentType: z.string().optional(),
  size: z.number().optional(),
  createdAt: z.string().optional(),
})

export const listArtifactsResponseSchema = z.object({
  artifacts: z.array(artifactMetadataSchema),
})

export type ArtifactMetadata = z.infer<typeof artifactMetadataSchema>
