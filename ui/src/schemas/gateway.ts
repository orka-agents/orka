export interface ObjectMeta {
  name: string
  namespace?: string
  uid?: string
  generation?: number
  creationTimestamp?: string
}

export interface GatewayCapabilities {
  inboundText?: boolean
  outboundText?: boolean
  threads?: boolean
  senderIdentity?: boolean
  explicitSessions?: boolean
  idempotentDelivery?: boolean
}

export interface Gateway {
  metadata: ObjectMeta
  spec: {
    gatewayClassName: string
    adapter: { endpoint?: string; serviceRef?: { name: string; port?: number } }
    metadata?: Record<string, string>
  }
  status?: {
    accepted?: boolean
    resolvedRefs?: boolean
    connected?: boolean
    ready?: boolean
    observedGeneration?: number
    resolvedEndpoint?: string
    observedCapabilities?: {
      contractVersion?: string
      adapterName?: string
      adapterVersion?: string
      capabilities?: GatewayCapabilities
    }
    lastSuccessfulProbe?: string
    message?: string
  }
}

export interface GatewayBinding {
  metadata: ObjectMeta
  spec: {
    gatewayRef: { name: string }
    agentRef: { name: string }
    match: { accountId: string; contextId: string; threadId?: string; senderId?: string }
    senderPolicy?: { mode?: 'allowlist' | 'all'; allowedSenderIds?: string[] }
    priority?: number
    session?: { mode?: string; name?: string }
    taskDefaults?: {
      priority?: number
      timeout?: string
      retryPolicy?: {
        maxRetries?: number
        backoffMultiplier?: number
        initialDelay?: string
      }
      agentRuntimeMaxTurns?: number
    }
    activeTurnBehavior?: 'queue'
  }
  status?: {
    accepted?: boolean
    resolvedRefs?: boolean
    programmed?: boolean
    ready?: boolean
    observedGeneration?: number
    resolvedCapabilities?: GatewayCapabilities
    lastInboundActivity?: string
    lastOutboundActivity?: string
    message?: string
    conditions?: Array<{
      type: string
      status: string
      reason?: string
      message?: string
      lastTransitionTime?: string
      observedGeneration?: number
    }>
  }
}

export type GatewayEventState =
  | 'Accepted' | 'Queued' | 'Dispatching' | 'TaskCreated' | 'Completed'
  | 'Rejected' | 'DeadLettered' | 'Expired'

export interface GatewayEvent {
  id: string
  namespace: string
  gatewayName: string
  bindingName?: string
  externalEventId: string
  state: GatewayEventState
  stateMessage?: string
  accountId: string
  contextId: string
  threadId?: string
  senderId: string
  text?: string
  replyTarget?: string
  sessionName?: string
  taskName?: string
  transcriptOrder?: number
  attemptCount: number
  receivedAt: string
  expiresAt: string
  createdAt: string
  updatedAt: string
}

export type GatewayDeliveryState =
  | 'Pending' | 'Sending' | 'Delivered' | 'RetryScheduled'
  | 'Failed' | 'DeadLettered' | 'Expired'

export interface GatewayDelivery {
  id: string
  namespace: string
  gatewayName: string
  bindingName?: string
  eventId: string
  taskName?: string
  sessionName?: string
  kind: 'final' | 'error'
  state: GatewayDeliveryState
  replyTarget: string
  text: string
  attemptCount: number
  maxAttempts: number
  manualRetryCount: number
  nextAttemptAt: string
  expiresAt: string
  providerMessageId?: string
  lastError?: string
  createdAt: string
  updatedAt: string
  deliveredAt?: string
}
