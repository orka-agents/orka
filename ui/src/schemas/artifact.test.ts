import { describe, it, expect } from 'vitest'
import { artifactMetadataSchema, listArtifactsResponseSchema } from './artifact'

describe('artifact schema', () => {
  it('parses full artifact metadata', () => {
    const a = artifactMetadataSchema.parse({
      filename: 'out.txt', contentType: 'text/plain', size: 42, createdAt: '2026-06-28T00:00:00Z',
    })
    expect(a.filename).toBe('out.txt')
  })

  it('parses minimal artifact with only filename', () => {
    expect(artifactMetadataSchema.parse({ filename: 'x' }).filename).toBe('x')
  })

  it('parses empty list response', () => {
    expect(listArtifactsResponseSchema.parse({ artifacts: [] }).artifacts).toEqual([])
  })
})
