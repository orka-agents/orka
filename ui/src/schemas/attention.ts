// Response shapes of GET /api/v1/attention and GET /api/v1/approvals. Items are
// short safe summaries; approval detail stays on the per-Task approvals API.

export type AttentionKind = 'approval' | 'outcomeUnknown' | 'failed' | 'podWaiting' | 'monitorBlocked'

export type AttentionApprovalSource = 'parked' | 'brokered' | 'worker'

export interface AttentionTaskRef {
  namespace: string
  name: string
  uid: string
  phase?: string
}

export interface AttentionApproval {
  id: string
  source: AttentionApprovalSource
  action?: string
  targetTool?: string
  severity?: string
  expiresAt?: string
  /** Hint only: true or false when settled, null when unknown. The decision POST enforces. */
  canApprove: boolean | null
  canDecline: boolean | null
}

export interface AttentionMonitor {
  name: string
  actionID: string
  targetKind?: string
  targetNumber?: number
  desiredAction?: string
  blockedReason?: string
}

export interface AttentionItem {
  id: string
  kind: AttentionKind
  rank: number
  since: string
  deadline?: string
  summary: string
  reason?: string
  task?: AttentionTaskRef
  approval?: AttentionApproval
  monitor?: AttentionMonitor
}

/**
 * Whether an optional source was read. partial (approvals only) means some
 * Tasks that may be waiting for approval could not be read by the caller.
 */
export type AttentionSourceState = 'included' | 'partial' | 'forbidden' | 'unavailable' | 'notRequested'

export interface AttentionResponse {
  namespace: string
  generatedAt: string
  since?: string
  items: AttentionItem[]
  total: number
  counts: Partial<Record<AttentionKind, number>>
  incomplete: boolean
  incompleteReasons?: string[]
  sources: { approvals: AttentionSourceState; monitors: AttentionSourceState }
  metadata: { continue?: string }
}
