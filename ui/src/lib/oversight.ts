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
