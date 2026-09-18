export interface UsageTotals {
  inputTokens: number
  outputTokens: number
  cachedInputTokens: number
  cacheWriteInputTokens: number
  cachedUsageReported: boolean
  cacheWriteUsageReported: boolean
  totalTokens: number
  estimatedTokens: number
  measurements: number
  reportedMeasurements: number
  missingMeasurements: number
  partialMeasurements: number
  calls: number
  attempts: number
  completeness: 'complete' | 'partial' | 'unavailable'
  modelCost: string
}

export interface UsageSummary extends UsageTotals {
  workRequests: number
  unfinishedWork: number
  prsOpened: number
  prsReady: number
  prsMerged: number
  prsClosed: number
  prsAssisted: number
  tokensPerPROpened: number | null
  tokensPerPRMerged: number | null
}

export interface UsageMeasurement {
  id: string
  attemptID?: string
  scope: string
  source: string
  provider?: string
  model?: string
  inputTokens: number | null
  outputTokens: number | null
  cachedInputTokens: number | null
  cacheWriteInputTokens: number | null
  status: string
  completeness: string
  gap?: string
  observedAt: string
}

export interface UsageTask {
  namespace: string
  taskUID: string
  taskName?: string
  sessionName?: string
  phase: string
  role?: string
  runtime?: string
  startedAt: string
  usage: UsageTotals
  measurements: UsageMeasurement[]
  shared: boolean
}

export interface UsagePR {
  namespace: string
  repository: string
  number: number
  url: string
  state: string
  origin: 'created' | 'assisted' | 'review_only'
  headSHA?: string
  ready: boolean
  readinessReason?: string
  mergedAt?: string
  observedAt: string
}

export interface UsageWorkSummary {
  id: string
  namespace: string
  monitorName: string
  repository: string
  kind: string
  number: number
  startedAt: string
  summary: UsageSummary
}

export interface UsageWork extends UsageWorkSummary {
  tasks?: UsageTask[]
  pullRequests?: UsagePR[]
  models?: string[]
}

export interface UsagePage {
  limit: number
  offset: number
  total: number
}

export interface UsageSelection {
  teams: string[]
  from: string
  until: string
  asOf: string
  repository?: string
  model?: string
  kind?: string
}

export interface UsageOtherSummary {
  category: string
  explanation: string
  usage: UsageTotals
  taskCount: number
}

export interface UsageOther extends UsageOtherSummary {
  tasks?: UsageTask[]
  page: UsagePage
}

export interface UsageReport {
  selection: UsageSelection
  retainedSince?: string
  summary: UsageSummary
  teams: { namespace: string; summary: UsageSummary }[]
  works: UsageWorkSummary[]
  otherWork: UsageOtherSummary[]
  page: UsagePage
}

export function usageNumber(value: number | null | undefined) {
  return value == null ? 'Unavailable' : Math.round(value).toLocaleString()
}

export function recordedTokens(usage: UsageTotals) {
  if (usage.measurements === 0) return 'No model calls'
  return usage.completeness === 'unavailable' ? 'Usage unavailable' : usageNumber(usage.totalTokens)
}
