import { z } from 'zod'

// Response shapes of the read-only oversight API (/api/v1/oversight/*). The
// Go types in pkg/oversight/v1 are the source of truth; every string here is
// data from the server and is rendered as plain text only.

export type OversightSignal = 'consistent' | 'risky' | 'against_charter' | 'monitor_unavailable'

export type OversightDisposition =
  | 'allow'
  | 'audit_flagged'
  | 'paused'
  | 'held_for_human'
  | 'fail_closed'
  | 'degraded'

export type OversightFeedbackLabel = 'real_problem' | 'false_alarm' | 'unclear'

export const OVERSIGHT_FEEDBACK_LABELS: readonly OversightFeedbackLabel[] = ['real_problem', 'false_alarm', 'unclear']

export interface CountedValue {
  value: string
  count: number
}

export interface EffectEvent {
  id: string
  kind: string
  binary?: string
  path?: string
  dest?: string
  args?: string
  outcome?: string
  role?: string
}

export interface ObservedEffects {
  counts?: Record<string, number>
  binaries?: CountedValue[]
  paths?: CountedValue[]
  destinations?: CountedValue[]
  blockedCount?: number
  /** Absent means the sensor did not observe sends; it is not zero. */
  bytesSent?: number
  notableEvents?: EffectEvent[]
  omittedEvents?: number
}

export interface ActionEnvelope {
  toolName: string
  toolCallID?: string
  requestSummary?: string
  effects: ObservedEffects
  priorActionIDs?: string[]
  taintFlags?: string[]
  inheritedTaint?: string[]
}

export interface ActionCoverage {
  streamState: string
  sensorState: string
  attributionMethod?: string
  attributionCounts?: Record<string, number>
  unattributedEvents?: number
  blindSpots?: string[]
}

export interface Tier0Evaluation {
  rules?: string[]
  highStakesClass?: string
}

export interface CharterFinding {
  producerRef: string
  boundID: string
  sentence: string
  citedEventIDs?: string[]
  enforced?: boolean
}

export interface ConcernScore {
  questionID: string
  label?: string
  probability: number
  threshold?: number
  triggered?: boolean
}

export interface ScorerEvaluation {
  providerRef: string
  modelID: string
  modelFamily?: string
  modelDigest?: string
  promptRevision?: string
  /** What the producer reported; Orka does not verify it in Phase 0. */
  trustState?: string
  windowSize?: number
  latencyMs?: number
  status: string
  citedEventIDs?: string[]
  scores?: ConcernScore[]
}

export interface AdvisorEvaluation {
  providerRef: string
  modelID: string
  modelFamily?: string
  modelDigest?: string
  promptRevision?: string
  trustState?: string
  status: string
  verdict?: string
  releaseRequested?: boolean
  releaseGranted?: boolean
  releaseDeniedReason?: string
  citedEventIDs?: string[]
  concerns?: string[]
  summary?: string
  latencyMs?: number
}

export interface DecisionFeedback {
  label: OversightFeedbackLabel
  note?: string
  reviewer?: string
  recordedAt: string
}

export interface DecisionView {
  id: string
  seq?: number
  namespace?: string
  taskName: string
  taskUID?: string
  agentName?: string
  workloadIdentity?: string
  runID?: string
  actionSeq: number
  observedAt: string
  recordedAt?: string
  mode: 'audit' | 'gate' | string
  outageFallback?: string
  charterRevision?: string
  concernsRef?: string
  concernsRevision?: string
  concernsDigest?: string
  policyRevision?: string
  action: ActionEnvelope
  evidenceLevel: string
  coverage: ActionCoverage
  tier0?: Tier0Evaluation
  charterFindings?: CharterFinding[]
  scorers?: ScorerEvaluation[]
  advisors?: AdvisorEvaluation[]
  disposition: OversightDisposition | string
  wouldHavePaused?: boolean
  escalatedBecause?: string[]
  approvalRef?: string
  signal: OversightSignal | string
  signatureHash?: string
  actionNarrative?: string
  feedback?: DecisionFeedback
}

export interface QueryTerm {
  key: string
  value: string
}

export interface DecisionListResponse {
  namespace: string
  query?: QueryTerm[]
  items: DecisionView[]
  continue?: string
}

export interface SignatureFeature {
  kind: string
  value: string
}

export interface SimilarItem {
  decision: DecisionView
  score: number
  exactSignature?: boolean
  shared: SignatureFeature[] | null
}

export interface SimilarResponse {
  namespace: string
  source: DecisionView
  signature: SignatureFeature[] | null
  items: SimilarItem[] | null
  searched: number
  truncated?: boolean
}

export interface DecisionGroup {
  key: string
  count: number
  risky: number
  againstCharter: number
  firstSeen: string
  lastSeen: string
  tasks?: string[]
}

export interface GroupsResponse {
  namespace: string
  by: 'agent' | 'task' | string
  query?: QueryTerm[]
  groups: DecisionGroup[] | null
  searched: number
  truncated?: boolean
}

export interface FeedbackResponse {
  recorded: number
  feedback: DecisionFeedback
}

export interface PostureState {
  state: string
  summary: string
  reasons?: string[]
}

export interface PostureCoverage {
  maxEvidenceLevel?: string
  streamOK: number
  sensorAttributed: number
  unattributedEvents: number
  /** Sum of effect counts across the window's actions; 0 when not reported. */
  observedEvents?: number
  blindSpots?: Record<string, number>
  /** Events a monitor deliberately left out of every action, by kind. */
  excluded?: Record<string, number>
}

export interface PostureBasis {
  actions: number
  tasks: number
  agents: number
  dispositions: Record<string, number>
  signals: Record<string, number>
  wouldHavePaused: number
  charterFindings: number
  unreviewedFlagged: number
  falseAlarms: number
  realProblems: number
  coverage: PostureCoverage
  lastObservedAt?: string
  /** Observed action → recorded by Orka; backfilled history is left out. */
  decisionLatency?: LatencySummary
}

export interface PostureProducer {
  role: string
  providerRef: string
  modelID: string
  trustState?: string
  evaluations: number
  failures: number
}

export interface PostureResponse {
  namespace: string
  window: string
  generatedAt: string
  mode?: string
  confidence: PostureState
  visibility: PostureState
  control: PostureState
  charterRevisions?: string[]
  concernsRevisions?: string[]
  basis: PostureBasis
  producers?: PostureProducer[]
  incomplete?: boolean
}

export interface ProducerMetric {
  name: string
  value: number
  unit?: string
}

export interface ProducerInfo {
  providerRef: string
  label?: string
  role?: string
  modelID?: string
  modelFamily?: string
  modelDigest?: string
  /** The declaration's claim; Orka has not verified it unless trustVerified. */
  declaredTrustState?: string
  trustVerified: boolean
  observedTrustStates?: string[]
  evalRunID?: string
  sampleSize?: number
  metrics?: ProducerMetric[]
  note?: string
  evaluations: number
  failures: number
}

export interface ProducersResponse {
  namespace: string
  declared: boolean
  producers: ProducerInfo[] | null
}

// The config is parsed, not just cast: every list defaults to empty so an
// older server that lacks a field still parses.
export const charterBoundSchema = z.object({
  id: z.string(),
  sentence: z.string().default(''),
  highlight: z.boolean().optional(),
})

export const charterDeclarationSchema = z.object({
  revision: z.string(),
  title: z.string().optional(),
  preview: z.boolean().optional(),
  bounds: z.array(charterBoundSchema).nullish().transform((v) => v ?? []),
})

export const contextLabelSchema = z.object({ name: z.string(), value: z.string() })

export const concernDeclarationSchema = z.object({
  id: z.string(),
  name: z.string(),
  meaning: z.string().optional(),
})

const list = <T extends z.ZodTypeAny>(item: T) => z.array(item).nullish().transform((v) => v ?? [])

export const oversightConfigSchema = z.object({
  queryKeys: list(z.string()),
  maxBulkFeedback: z.number().default(200),
  /** Charter revisions an operator marked as a demo (preview) charter. */
  demoCharterRevisions: list(z.string()),
  charters: list(charterDeclarationSchema),
  /** Operator-declared facts about the environment; Orka checks none of them. */
  context: list(contextLabelSchema),
  /** The names of the questions the monitor asks. */
  concerns: list(concernDeclarationSchema),
})

export type CharterBound = z.infer<typeof charterBoundSchema>
export type CharterDeclaration = z.infer<typeof charterDeclarationSchema>
export type ContextLabel = z.infer<typeof contextLabelSchema>
export type ConcernDeclaration = z.infer<typeof concernDeclarationSchema>
/** The config as parsed: every list is present. */
export type OversightConfig = z.output<typeof oversightConfigSchema>

/** How long decisions took from the observed action to being recorded; absent on an older server or with no live records. */
export const latencySummarySchema = z.object({
  count: z.number(),
  p50Seconds: z.number(),
  p95Seconds: z.number(),
  maxSeconds: z.number(),
})

export type LatencySummary = z.infer<typeof latencySummarySchema>

/** Posture coverage as parsed: observedEvents defaults to 0 on an older server. */
export const postureCoverageSchema = z.object({
  maxEvidenceLevel: z.string().optional(),
  streamOK: z.number().default(0),
  sensorAttributed: z.number().default(0),
  unattributedEvents: z.number().default(0),
  observedEvents: z.number().default(0),
  blindSpots: z.record(z.number()).optional(),
  excluded: z.record(z.number()).optional(),
})

/** Cheap shape check for a decision frame read off the stream. */
export function isDecisionView(value: unknown): value is DecisionView {
  if (!value || typeof value !== 'object') return false
  const record = value as Record<string, unknown>
  return typeof record.id === 'string' && typeof record.taskName === 'string' &&
    typeof record.action === 'object' && record.action !== null &&
    typeof record.coverage === 'object' && record.coverage !== null
}
