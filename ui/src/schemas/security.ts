import { z } from 'zod'
import { agentRefSchema, k8sMetadataSchema } from './task'

export const findingCountsSchema = z.object({
  total: z.number().optional(),
  critical: z.number().optional(),
  high: z.number().optional(),
  medium: z.number().optional(),
  low: z.number().optional(),
})

export const repositoryScanSpecSchema = z.object({
  provider: z.string().optional(),
  repoURL: z.string(),
  owner: z.string().optional(),
  repository: z.string().optional(),
  branch: z.string().optional(),
  subPath: z.string().optional(),
  gitSecretRef: z.object({ name: z.string() }).optional(),
  forkRepo: z.string().optional(),
  prBaseBranch: z.string().optional(),
  schedule: z.string().optional(),
  timeZone: z.string().optional(),
  historyDays: z.number().optional(),
  validationMode: z.string().optional(),
  validationMaxFindingsPerRun: z.number().optional(),
  validationMinSeverity: z.string().optional(),
  validationMinConfidence: z.string().optional(),
  customScanInstructionsRef: z.object({ name: z.string(), key: z.string().optional() }).optional(),
  falsePositivePolicyRef: z.object({ name: z.string(), key: z.string().optional() }).optional(),
  analysisAgentRef: agentRefSchema,
  patchAgentRef: agentRefSchema.optional(),
  maxFindingsPerRun: z.number().optional(),
  suspend: z.boolean().optional(),
})

export const repositoryScanStatusSchema = z.object({
  phase: z.string().optional(),
  lastScanID: z.string().optional(),
  lastScanTaskName: z.string().optional(),
  lastScanAt: z.string().optional(),
  lastSuccessfulScanAt: z.string().optional(),
  lastObservedHeadSHA: z.string().optional(),
  lastProcessedCommit: z.string().optional(),
  threatModelVersion: z.number().optional(),
  findingCounts: findingCountsSchema.optional(),
})

export const repositoryScanSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: k8sMetadataSchema,
  spec: repositoryScanSpecSchema,
  status: repositoryScanStatusSchema.optional(),
})

export const scanRunSchema = z.object({
  id: z.string(),
  namespace: z.string(),
  repositoryScan: z.string(),
  taskName: z.string(),
  mode: z.string(),
  phase: z.string(),
  startedAt: z.string(),
  completedAt: z.string().optional(),
  baseCommit: z.string().optional(),
  headCommit: z.string().optional(),
  commitCount: z.number().optional(),
  sliceCount: z.number().optional(),
  reviewedSliceCount: z.number().optional(),
  skippedSliceCount: z.number().optional(),
  acceptedFindings: z.number().optional(),
  droppedFindings: z.number().optional(),
  scannerPolicyVersion: z.string().optional(),
  policyDigest: z.string().optional(),
  idempotencyKey: z.string().optional(),
  summary: z.string().optional(),
  errorMessage: z.string().optional(),
})

export const threatModelSchema = z.object({
  namespace: z.string(),
  repositoryScan: z.string(),
  version: z.number(),
  content: z.string(),
  source: z.string(),
  generatedByScan: z.string().optional(),
  createdAt: z.string(),
  updatedAt: z.string(),
})

export const findingEvidenceRefSchema = z.object({
  kind: z.string(),
  taskName: z.string().optional(),
  name: z.string().optional(),
  label: z.string().optional(),
  path: z.string().optional(),
  startLine: z.number().optional(),
  endLine: z.number().optional(),
  symbol: z.string().optional(),
  quote: z.string().optional(),
})

export const securityFindingSchema = z.object({
  id: z.string(),
  namespace: z.string(),
  repositoryScan: z.string(),
  scanRunID: z.string().optional(),
  scanTaskName: z.string().optional(),
  sliceID: z.string().optional(),
  fingerprint: z.string(),
  title: z.string(),
  category: z.string().optional(),
  summary: z.string(),
  severity: z.string(),
  confidence: z.string(),
  triage: z.string().optional(),
  validationStatus: z.string(),
  state: z.string(),
  filePath: z.string().optional(),
  line: z.number().optional(),
  commitSHA: z.string().optional(),
  rootCause: z.string().optional(),
  reproduction: z.string().optional(),
  remediation: z.string().optional(),
  suggestedAction: z.string().optional(),
  whyTestsDoNotAlreadyCoverThis: z.string().optional(),
  suggestedRegressionTest: z.string().optional(),
  minimumFixScope: z.string().optional(),
  evidence: z.array(findingEvidenceRefSchema).optional(),
  validationJSON: z.string().optional(),
  patchProposalID: z.string().optional(),
  prNumber: z.number().optional(),
  prURL: z.string().optional(),
  createdAt: z.string(),
  updatedAt: z.string(),
})

export const reviewSliceFileSchema = z.object({
  path: z.string(),
  reason: z.string().optional(),
  symbol: z.string().optional(),
  route: z.string().optional(),
  command: z.string().optional(),
})

export const reviewSliceTestSchema = z.object({
  path: z.string(),
  command: z.string().optional(),
})

export const reviewSliceSchema = z.object({
  schemaVersion: z.number().optional(),
  id: z.string(),
  namespace: z.string().optional(),
  repositoryScan: z.string(),
  source: z.string(),
  title: z.string(),
  summary: z.string().optional(),
  kind: z.string(),
  entrypoints: z.array(reviewSliceFileSchema).optional(),
  ownedFiles: z.array(reviewSliceFileSchema).optional(),
  contextFiles: z.array(reviewSliceFileSchema).optional(),
  tests: z.array(reviewSliceTestSchema).optional(),
  tags: z.array(z.string()).optional(),
  trustBoundaries: z.array(z.string()).optional(),
  confidence: z.string(),
  status: z.string(),
  lastScanRunID: z.string().optional(),
  lastReviewedAt: z.string().optional(),
  createdAt: z.string().optional(),
  updatedAt: z.string().optional(),
})

export const droppedFindingSchema = z.object({
  id: z.string(),
  namespace: z.string(),
  repositoryScan: z.string(),
  scanRunID: z.string(),
  taskName: z.string(),
  sliceID: z.string().optional(),
  reason: z.string(),
  layer: z.string().optional(),
  sampleJSON: z.string().optional(),
  createdAt: z.string(),
})

export const patchProposalSchema = z.object({
  id: z.string(),
  namespace: z.string(),
  repositoryScan: z.string(),
  findingID: z.string(),
  taskName: z.string(),
  branch: z.string(),
  diffArtifact: z.string().optional(),
  summaryArtifact: z.string().optional(),
  status: z.string(),
  prNumber: z.number().optional(),
  prURL: z.string().optional(),
  createdAt: z.string(),
  updatedAt: z.string(),
})

export type RepositoryScan = z.infer<typeof repositoryScanSchema>
export type ScanRun = z.infer<typeof scanRunSchema>
export type ThreatModel = z.infer<typeof threatModelSchema>
export type SecurityFinding = z.infer<typeof securityFindingSchema>
export type PatchProposal = z.infer<typeof patchProposalSchema>
export type ReviewSlice = z.infer<typeof reviewSliceSchema>
export type DroppedFinding = z.infer<typeof droppedFindingSchema>
