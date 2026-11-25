import { stringify } from 'yaml'
import type {
  CharterBound,
  CharterDeclaration,
  ConcernScore,
  OversightConfig,
  DecisionView,
  OversightFeedbackLabel,
  OversightSignal,
  ProducerInfo,
  QueryTerm,
  SignatureFeature,
} from '@/schemas/oversight'

/** Evaluation statuses without a usable answer: the action would be held. */
export const MONITOR_FAILURE_STATUSES = new Set(['unavailable', 'timeout', 'error', 'content_filtered'])

export const isMonitorFailure = (status: string) => status !== 'ok'

export interface SignalPresentation {
  label: string
  /** Visual tone; never the only signal, the label always says it too. */
  tone: 'muted' | 'amber' | 'red' | 'red-outline'
}

export const SIGNAL_PRESENTATION: Record<OversightSignal, SignalPresentation> = {
  consistent: { label: 'Consistent with the task', tone: 'muted' },
  risky: { label: 'Looks risky', tone: 'amber' },
  against_charter: { label: 'Against the charter', tone: 'red' },
  monitor_unavailable: { label: 'Scorer unavailable → would hold', tone: 'red-outline' },
}

export function signalPresentation(signal: string): SignalPresentation {
  return SIGNAL_PRESENTATION[signal as OversightSignal] ?? { label: signal || 'Unclassified', tone: 'muted' }
}

export const FEEDBACK_LABEL_TEXT: Record<OversightFeedbackLabel, string> = {
  real_problem: 'Real problem',
  false_alarm: 'False alarm',
  unclear: 'Unclear',
}

export function feedbackLabelText(label: string): string {
  return FEEDBACK_LABEL_TEXT[label as OversightFeedbackLabel] ?? label
}

export const formatProbability = (value: number) => (Number.isFinite(value) ? value.toFixed(2) : '?')

export interface TopScore extends ConcernScore {
  providerRef: string
}

/**
 * The score that best explains a risky row: the highest triggered score, else
 * the highest score at or over its threshold.
 */
export function topTriggeredScore(decision: DecisionView): TopScore | undefined {
  let best: TopScore | undefined
  let bestRank = -1
  for (const scorer of decision.scorers ?? []) {
    for (const score of scorer.scores ?? []) {
      const over = score.threshold !== undefined && score.probability >= score.threshold
      const rank = score.triggered ? 2 : over ? 1 : 0
      if (rank === 0) continue
      if (rank > bestRank || (rank === bestRank && best && score.probability > best.probability)) {
        best = { ...score, providerRef: scorer.providerRef }
        bestRank = rank
      }
    }
  }
  return best
}

/** "K2 0.91 ≥ 0.82", or "K2 0.91" without a threshold. */
export function scoreVsThreshold(score: ConcernScore): string {
  const p = formatProbability(score.probability)
  if (score.threshold === undefined) return `${score.questionID} ${p}`
  const op = score.probability >= score.threshold ? '≥' : '<'
  return `${score.questionID} ${p} ${op} ${formatProbability(score.threshold)}`
}

/** Concern and question IDs seen in a set of decisions' scores, sorted. */
export function questionColumns(decisions: DecisionView[]): string[] {
  const ids = new Set<string>()
  for (const decision of decisions) {
    for (const scorer of decision.scorers ?? []) {
      for (const score of scorer.scores ?? []) if (score.questionID) ids.add(score.questionID)
    }
    for (const advisor of decision.advisors ?? []) {
      for (const concern of advisor.concerns ?? []) if (concern) ids.add(concern)
    }
  }
  return [...ids].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))
}

export interface MatrixCell {
  probability?: number
  threshold?: number
  triggered: boolean
  /** An advisor named the concern without a probability. */
  advisorNamed: boolean
}

/** The highest score any scorer gave one question, plus advisor mentions. */
export function matrixCell(decision: DecisionView, questionID: string): MatrixCell {
  const cell: MatrixCell = { triggered: false, advisorNamed: false }
  for (const scorer of decision.scorers ?? []) {
    for (const score of scorer.scores ?? []) {
      if (score.questionID !== questionID) continue
      if (cell.probability === undefined || score.probability > cell.probability) {
        cell.probability = score.probability
        cell.threshold = score.threshold
      }
      const over = score.threshold !== undefined && score.probability >= score.threshold
      if (score.triggered || over) cell.triggered = true
    }
  }
  for (const advisor of decision.advisors ?? []) {
    if (advisor.concerns?.includes(questionID)) {
      cell.advisorNamed = true
      cell.triggered = true
    }
  }
  return cell
}

// --- Search grammar helpers -------------------------------------------------

/**
 * One key:value term. A value with whitespace is quoted (the grammar keeps
 * the key for key:"quoted value"); double quotes cannot be escaped, so they
 * are dropped.
 */
export function queryTerm(key: string, value: string): string {
  const clean = value.replace(/"/g, '').trim()
  return /\s/.test(clean) ? `${key}:"${clean}"` : `${key}:${clean}`
}

/** Rebuilds a query from the terms the server echoed. */
export function termsToQuery(terms: QueryTerm[]): string {
  return terms
    .map((term) => (term.key === 'text' ? (/\s/.test(term.value) ? `"${term.value.replace(/"/g, '')}"` : term.value) : queryTerm(term.key, term.value)))
    .join(' ')
}

/** The query without the term at index. */
export function removeTerm(terms: QueryTerm[], index: number): string {
  return termsToQuery(terms.filter((_, i) => i !== index))
}

/** Base name of a binary path, like the server's signature. */
export function binaryName(binary: string): string {
  const trimmed = binary.trim().replace(/\/+$/, '')
  const slash = trimmed.lastIndexOf('/')
  return slash >= 0 ? trimmed.slice(slash + 1) : trimmed
}

/** The directory of an absolute path as a glob ("/etc/passwd" → "/etc/*"), or undefined. */
export function pathDirectoryGlob(p: string): string | undefined {
  const trimmed = p.trim()
  if (!trimmed.startsWith('/')) return undefined
  const slash = trimmed.replace(/\/+$/, '').lastIndexOf('/')
  if (slash <= 0) return undefined
  return `${trimmed.slice(0, slash)}/*`
}

/** A destination host's last two DNS labels, or undefined for an IP address. */
export function destinationDomain(dest: string): string | undefined {
  let host = dest.trim()
  if (host.startsWith('[')) return undefined
  const colon = host.lastIndexOf(':')
  if (colon > 0 && host.indexOf(':') === colon) host = host.slice(0, colon)
  host = host.toLowerCase().replace(/\.$/, '')
  if (!host || /^[\d.]+$/.test(host) || host.includes(':')) return undefined
  const labels = host.split('.')
  return labels.length <= 2 ? host : labels.slice(-2).join('.')
}

/** Maps a signature feature kind to the search key that matches it. */
export const FEATURE_QUERY_KEY: Record<string, string> = {
  tool: 'tool',
  effect: 'effect',
  bin: 'bin',
  path: 'path',
  dest: 'dest',
  dest_domain: 'domain',
  concern: 'concern',
  taint: 'taint',
}

/** Why a similar action matched, in words: "same tool code_exec", "concern K10". */
export function featureReason(feature: SignatureFeature): string {
  if (feature.kind === 'tool') return `same tool ${feature.value}`
  return `${feature.kind} ${feature.value}`
}

// --- Producers ----------------------------------------------------------------

/** The operator's label for a producer, else its providerRef. */
export function producerLabel(providerRef: string, producers: ProducerInfo[] | undefined): string {
  const declared = producers?.find((p) => p.providerRef === providerRef)
  return declared?.label?.trim() || providerRef
}

// --- Posture reasons ----------------------------------------------------------

const REASON_TEXT: Record<string, string> = {
  no_decisions_in_window: 'No decisions were recorded in this window',
  event_stream_missing: 'An action event stream was missing',
  event_stream_partial: 'An action event stream was partial',
  sensor_unavailable: 'The effect sensor was unavailable',
  sensor_saw_nothing: 'The effect sensor saw nothing for some actions',
  unattributed_events_present: 'Some sensor events could not be attributed to an action',
  known_blind_spots_active: 'Known blind spots are active',
  effects_not_observed: 'Some effects were not independently observed',
  monitor_failures: 'A scorer or advisor gave no usable answer',
  untrusted_monitor: 'A monitor is in the untrusted state',
  audit_mode_only: 'Audit mode only: nothing is held',
  canaries_not_reported: 'No canary evidence is reported yet',
  against_charter: 'An action went against the charter',
  held_or_fail_closed: 'An action was held or failed closed',
  risky_unreviewed: 'Risky actions await review',
  visibility_not_warranted: 'Coverage is not full',
  window_not_fully_read: 'The window held more decisions than were read',
}

/** A reason code in words; unknown codes are shown as sent. */
export function reasonText(code: string): string {
  return REASON_TEXT[code] ?? code
}

// --- Draft concern -------------------------------------------------------------

/**
 * A concern YAML draft for copying: built entirely in the browser, it changes
 * nothing on the server. Examples come from the selected rows' redacted
 * narratives; not_examples from rows people labelled false alarms.
 */
export function draftConcernYaml(query: string, selected: DecisionView[], resultSet: DecisionView[]): string {
  const narrative = (d: DecisionView) => d.actionNarrative?.trim() || `${d.action.toolName} (no narrative)`
  const falseAlarms = resultSet.filter((d) => d.feedback?.label === 'false_alarm')
  const falseAlarmIDs = new Set(falseAlarms.map((d) => d.id))
  const examples = [...new Set(selected.filter((d) => !falseAlarmIDs.has(d.id)).map(narrative))]
  const notExamples = [...new Set(falseAlarms.map(narrative))]
  const draft = {
    id: 'CONCERN-ID',
    description: query.trim() ? `Actions matching: ${query.trim()}` : 'Describe the behaviour this concern watches for',
    examples,
    not_examples: notExamples,
  }
  return `# Draft only: replace the id and review every line before use.\n${stringify(draft)}`
}

// --- Charters -----------------------------------------------------------------

/** Revisions labelled "Demo charter": listed by the config, or a preview charter. */
export function demoCharterRevisions(config: OversightConfig | undefined): string[] {
  const out = new Set(config?.demoCharterRevisions ?? [])
  for (const charter of config?.charters ?? []) if (charter.preview) out.add(charter.revision)
  return [...out]
}

export const isDemoCharter = (charter: CharterDeclaration, config: OversightConfig | undefined) =>
  Boolean(charter.preview) || (config?.demoCharterRevisions ?? []).includes(charter.revision)

export interface NumberedBound extends CharterBound {
  /** 1-based position in the charter as declared; stable whatever is highlighted. */
  number: number
}

/** A charter's bounds numbered in declared order, highlighted bounds first. */
export function orderedBounds(charter: CharterDeclaration): NumberedBound[] {
  const numbered = (charter.bounds ?? []).map((bound, i) => ({ ...bound, number: i + 1 }))
  return [...numbered.filter((b) => b.highlight), ...numbered.filter((b) => !b.highlight)]
}

export interface CharterRule {
  charter: CharterDeclaration
  bound: NumberedBound
}

/**
 * The declared rule a finding names: the bound in the decision's own charter
 * revision first, else in any declared charter. Undefined when undeclared.
 */
export function findCharterRule(config: OversightConfig | undefined, revision: string | undefined, boundID: string): CharterRule | undefined {
  const charters = config?.charters ?? []
  const ordered = [...charters.filter((c) => c.revision === revision), ...charters.filter((c) => c.revision !== revision)]
  for (const charter of ordered) {
    const index = (charter.bounds ?? []).findIndex((b) => b.id === boundID)
    if (index >= 0) return { charter, bound: { ...charter.bounds![index], number: index + 1 } }
  }
  return undefined
}

/** "Rule 2", or the bound ID when the charter does not declare it. */
export const ruleName = (rule: CharterRule | undefined, boundID: string) => (rule ? `Rule ${rule.bound.number}` : `rule ${boundID}`)

export const charterName = (charter: CharterDeclaration) => charter.title?.trim() || charter.revision

// --- Plain-language rows and incidents -----------------------------------------

const SYNTHETIC_PREFIX = /^\s*\[synthetic\]\s*/i

/** Whether a producer marked a text as synthetic (generated for a demo or test). */
export const isSyntheticText = (text: string | undefined) => Boolean(text && SYNTHETIC_PREFIX.test(text))

/** A text without its leading "[synthetic] " marker, for row display only. */
export const stripSynthetic = (text: string) => text.replace(SYNTHETIC_PREFIX, '')

/** 2.6 MB, 512 KB, 120 B. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '?'
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1).replace(/\.0$/, '')} MB`
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1).replace(/\.0$/, '')} KB`
  return `${bytes} B`
}

const hasBytes = (decision: DecisionView) => typeof decision.action.effects?.bytesSent === 'number'

/** What the action did, from its fields: bytes and destination, else the narrative, else its effects. */
export function actionPhrase(decision: DecisionView): string {
  const effects = decision.action.effects ?? {}
  const dest = effects.destinations?.[0]?.value
  if (dest && hasBytes(decision)) return `sent ${formatBytes(effects.bytesSent!)} to ${dest}`
  if (dest) return `connected to ${dest}`
  const narrative = decision.actionNarrative?.trim()
  if (narrative) return stripSynthetic(narrative)
  const path = effects.paths?.[0]?.value
  if (path) return `touched ${path}`
  const binary = effects.binaries?.[0]?.value
  if (binary) return `ran ${binaryName(binary)}`
  return `called ${decision.action.toolName}`
}

/** "sent 2.6 MB to host:443 — breaks charter rule 1". */
export function rowSummary(decision: DecisionView, config: OversightConfig | undefined): string {
  const phrase = actionPhrase(decision)
  const finding = decision.charterFindings?.[0]
  if (finding) {
    const rule = findCharterRule(config, decision.charterRevision, finding.boundID)
    return `${phrase} — breaks charter rule ${rule ? rule.bound.number : finding.boundID}`
  }
  if (decision.signal === 'monitor_unavailable' || decision.disposition === 'fail_closed') return `${phrase} — scorer unavailable, would hold`
  if (decision.wouldHavePaused) return `${phrase} — would pause`
  return phrase
}

/** The kernel-observed fact that best backs the story, or undefined when nothing was observed. */
export function strongestEvidence(decision: DecisionView): string | undefined {
  const effects = decision.action.effects ?? {}
  const dest = effects.destinations?.[0]?.value
  if (hasBytes(decision)) return `kernel saw ${formatBytes(effects.bytesSent!)} sent${dest ? ` to ${dest}` : ''}`
  const event = effects.notableEvents?.[0]
  if (event) {
    const what = event.dest || event.path || (event.binary && binaryName(event.binary))
    return `kernel saw ${event.kind}${what ? ` ${what}` : ''}${event.outcome ? ` (${event.outcome})` : ''}`
  }
  return undefined
}

export type RowBadgeKind = 'against_charter' | 'would_hold' | 'would_pause' | 'risky' | 'consistent'

const ROW_BADGES: Record<RowBadgeKind, SignalPresentation & { rank: number }> = {
  against_charter: { label: 'Against the charter', tone: 'red', rank: 4 },
  would_hold: { label: 'Scorer unavailable → would hold', tone: 'red-outline', rank: 3 },
  would_pause: { label: 'Would pause (audit mode)', tone: 'amber', rank: 2 },
  risky: { label: 'Looks risky', tone: 'amber', rank: 1 },
  consistent: { label: 'Consistent with the task', tone: 'muted', rank: 0 },
}

/** The one most severe badge for a decision. */
export function rowBadgeKind(decision: DecisionView): RowBadgeKind {
  if (decision.signal === 'against_charter') return 'against_charter'
  if (decision.signal === 'monitor_unavailable' || decision.disposition === 'fail_closed') return 'would_hold'
  if (decision.wouldHavePaused) return 'would_pause'
  if (decision.signal === 'risky') return 'risky'
  return 'consistent'
}

export const rowBadge = (kind: RowBadgeKind) => ROW_BADGES[kind]

/** The hover text that keeps raw identifiers out of the row itself. */
export function rowTooltip(decision: DecisionView): string {
  return [
    `tool ${decision.action.toolName}`,
    decision.action.requestSummary && `request: ${decision.action.requestSummary}`,
    `task ${decision.taskName}`,
    decision.agentName && `agent ${decision.agentName}`,
    `id ${decision.id}`,
  ].filter(Boolean).join('\n')
}

/** Whether a decision needs a person: a charter break, a would-pause or a would-hold. */
export const needsHuman = (decision: DecisionView) => rowBadgeKind(decision) !== 'risky' && rowBadgeKind(decision) !== 'consistent'

export interface Incident {
  key: string
  actor: string
  /** A bound ID for a charter break, else "would-pause" or "would-hold". */
  kind: string
  badge: RowBadgeKind
  title: string
  decisions: DecisionView[]
  lastSeen: string
}

const shorten = (text: string, max = 90) => (text.length > max ? `${text.slice(0, max - 1).trimEnd()}…` : text)

/**
 * Groups the decisions that need a person into incidents by actor (agent,
 * else Task) and kind, most severe first and then most recent. Titles are
 * built from the data: the actor, then the broken rule or the newest
 * decision's own account of what it did.
 */
export function groupIncidents(decisions: DecisionView[], config: OversightConfig | undefined): Incident[] {
  const groups = new Map<string, Incident>()
  const newestFirst = [...decisions].sort((a, b) => (b.seq ?? 0) - (a.seq ?? 0) || b.observedAt.localeCompare(a.observedAt))
  for (const decision of newestFirst) {
    if (!needsHuman(decision)) continue
    const badge = rowBadgeKind(decision)
    const finding = decision.charterFindings?.[0]
    const kind = badge === 'against_charter' && finding ? finding.boundID : badge === 'would_pause' ? 'would-pause' : 'would-hold'
    const actor = decision.agentName || decision.taskName
    const key = `${actor}\u0000${kind}`
    const existing = groups.get(key)
    if (existing) {
      existing.decisions.push(decision)
      continue
    }
    let title: string
    if (finding) {
      const rule = findCharterRule(config, decision.charterRevision, finding.boundID)
      const sentence = rule?.bound.sentence ?? finding.sentence
      title = `${actor} broke charter rule ${rule ? rule.bound.number : finding.boundID}: ${shorten(sentence)}`
    } else {
      title = `${actor} ${shorten(actionPhrase(decision))}`
    }
    groups.set(key, { key, actor, kind, badge, title, decisions: [decision], lastSeen: decision.observedAt })
  }
  return [...groups.values()].sort((a, b) => rowBadge(b.badge).rank - rowBadge(a.badge).rank || b.lastSeen.localeCompare(a.lastSeen))
}
