import { z } from 'zod'

// Mirrors internal/store ArtifactMetadata (JSON tags filename/contentType/size/
// createdAt). The list endpoint returns { artifacts: [] }; downloads are served
// from /tasks/:id/artifacts/:filename.
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
export type ListArtifactsResponse = z.infer<typeof listArtifactsResponseSchema>
