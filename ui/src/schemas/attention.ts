
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
