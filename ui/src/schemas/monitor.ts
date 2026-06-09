import { z } from 'zod'
import { agentRefSchema, k8sMetadataSchema } from './task'

export const repositoryMonitorTargetSchema = z.object({
  enabled: z.boolean().optional(),
  includeDrafts: z.boolean().optional(),
  maxPerRun: z.number().optional(),
})

export const repositoryMonitorSpecSchema = z.object({
  provider: z.string().optional(),
  repoURL: z.string(),
  owner: z.string().optional(),
  repository: z.string().optional(),
  branch: z.string().optional(),
  gitSecretRef: z.object({ name: z.string() }).optional(),
  schedule: z.string().optional(),
  timeZone: z.string().optional(),
  suspend: z.boolean().optional(),
  targets: z.object({
    pullRequests: repositoryMonitorTargetSchema.optional(),
    issues: repositoryMonitorTargetSchema.optional(),
    commits: repositoryMonitorTargetSchema.optional(),
  }).optional(),
  agents: z.object({
    reviewer: agentRefSchema.optional(),
    repairer: agentRefSchema.optional(),
    implementer: agentRefSchema.optional(),
  }).optional(),
  review: z.object({
    event: z.string().optional(),
    requireGreenCI: z.boolean().optional(),
    exactEventEnabled: z.boolean().optional(),
  }).optional(),
  repair: z.object({
    enabled: z.boolean().optional(),
    requireMaintainerOptIn: z.boolean().optional(),
  }).optional(),
  automerge: z.object({
    enabled: z.boolean().optional(),
    requireMaintainerOptIn: z.boolean().optional(),
    requireGlobalMergeGate: z.boolean().optional(),
    allowedMergeMethods: z.array(z.string()).optional(),
  }).optional(),
  validation: z.object({
    mode: z.string().optional(),
    commands: z.array(z.string()).optional(),
  }).optional(),
})

export const repositoryMonitorStatusSchema = z.object({
  phase: z.string().optional(),
  lastRunID: z.string().optional(),
  lastRunTime: z.string().optional(),
  lastSuccessfulRunTime: z.string().optional(),
  observedGeneration: z.number().optional(),
  openPullRequests: z.number().optional(),
  pendingReviews: z.number().optional(),
  activeRepairs: z.number().optional(),
  blockedItems: z.number().optional(),
  mergeReadyItems: z.number().optional(),
})

export const repositoryMonitorSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: k8sMetadataSchema,
  spec: repositoryMonitorSpecSchema,
  status: repositoryMonitorStatusSchema.optional(),
})

export const monitorRunSchema = z.object({
  id: z.string(),
  monitorNamespace: z.string(),
  monitorName: z.string(),
  trigger: z.string(),
  targetKind: z.string().optional(),
  targetNumber: z.number().optional(),
  targetSHA: z.string().optional(),
  phase: z.string(),
  startedAt: z.string(),
  completedAt: z.string().optional(),
  selectedCount: z.number().optional(),
  createdTaskCount: z.number().optional(),
  skippedCount: z.number().optional(),
  error: z.string().optional(),
})

export const monitorItemSchema = z.object({
  monitorNamespace: z.string(),
  monitorName: z.string(),
  kind: z.string(),
  itemKey: z.string(),
  number: z.number().optional(),
  sha: z.string().optional(),
  title: z.string().optional(),
  author: z.string().optional(),
  state: z.string().optional(),
  baseBranch: z.string().optional(),
  headBranch: z.string().optional(),
  headSHA: z.string().optional(),
  ciState: z.string().optional(),
  skipReason: z.string().optional(),
  lastVerdict: z.string().optional(),
  repairState: z.string().optional(),
  automergeState: z.string().optional(),
  updatedAt: z.string(),
  lastSeenAt: z.string(),
})

export type RepositoryMonitor = z.infer<typeof repositoryMonitorSchema>
export type MonitorRun = z.infer<typeof monitorRunSchema>
export type MonitorItem = z.infer<typeof monitorItemSchema>
