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
export function featureReason(feature: SignatureFeature, config?: OversightConfig): string {
  if (feature.kind === 'tool') return `same tool ${feature.value}`
  if (feature.kind === 'concern') return `concern ${concernName(feature.value, config)}`
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
/** "Rule 2"; a bound the declared charter does not list is an undeclared rule, never its bare ID. */
export const ruleName = (rule: CharterRule | undefined) => (rule ? `Rule ${rule.bound.number}` : 'Undeclared rule')

/**
 * A concern's display name: the operator's declared name, else the score's
 * own label, else the ID as a last resort.
 */
export function concernName(id: string, config: OversightConfig | undefined, label?: string): string {
  return config?.concerns?.find((c) => c.id === id)?.name?.trim() || label?.trim() || id
}

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

export interface ImageRef {
  host: string
  /** The repository path without its last element; empty for host/name:tag. */
  path: string
  name: string
  tag: string
}

const IMAGE_REF = /^([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::\d+)?)\/((?:[a-z0-9._-]+\/)*)([a-z0-9._-]+):([\w][\w.-]{0,127})$/i

/**
 * The last argument that reads as a registry image reference
 * (host/.../name:tag, or anything after oci://). The host must look like a
 * host (a dot, a port or localhost), so "dir/file:line" is not taken.
 */
export function parseImageRef(args: string | undefined): ImageRef | undefined {
  if (!args) return undefined
  const tokens = args.split(/\s+/).map((t) => t.replace(/^['"]|['"]$/g, '')).filter(Boolean)
  for (let i = tokens.length - 1; i >= 0; i--) {
    const oci = tokens[i].startsWith('oci://')
    const ref = oci ? tokens[i].slice('oci://'.length) : tokens[i]
    const match = IMAGE_REF.exec(ref)
    if (!match) continue
    const [, host, dirs, name, tag] = match
    if (!oci && !/[.:]/.test(host) && host !== 'localhost') continue
    return { host, path: dirs.replace(/\/$/, ''), name, tag }
  }
  return undefined
}

/** The path of a raw HTTP PUT in an event's args ("PUT /v2/x/blobs/uploads/"). */
export function parsePutPath(args: string | undefined): string | undefined {
  const match = args ? /\bPUT\s+(\/\S*)/.exec(args) : null
  return match ? match[1] : undefined
}

const hostOf = (dest: string) => {
  const trimmed = dest.trim()
  const colon = trimmed.lastIndexOf(':')
  return colon > 0 && trimmed.indexOf(':') === colon ? trimmed.slice(0, colon) : trimmed
}

/**
 * What the action did, built from fields only, never from the free-text
 * narrative (which can carry Task names and markers mid-string): bytes and
 * destination when a send was seen, else "<agent> ran <binary> — <request>".
 * The Task name never appears.
 */
export function actionPhrase(decision: DecisionView, { bytes = true }: { bytes?: boolean } = {}): string {
  return describeAction(decision, bytes).text
}

/** The OCI distribution path without its /v2/ prefix and blob or manifest suffix: "/v2/prod/api/blobs/uploads/" → "prod/api". */
export function distributionRepo(path: string): string {
  return path
    .replace(/^\/+/, '')
    .replace(/^v2\//, '')
    .replace(/\/(?:blobs\/uploads(?:\/[^/]*)?|manifests\/[^/]+)\/?$/, '')
    .replace(/\/+$/, '')
}

type ActionKind = 'push' | 'upload' | 'credential' | 'send' | 'connect' | 'ran'

/** How descriptive each phrase is, for choosing an incident's title. */
const ACTION_RANK: Record<ActionKind, number> = { push: 5, upload: 4, credential: 3, send: 2, connect: 1, ran: 0 }

function describeAction(decision: DecisionView, bytes: boolean): { kind: ActionKind; text: string } {
  const effects = decision.action.effects ?? {}
  const dest = effects.destinations?.[0]?.value
  const size = bytes && hasBytes(decision) ? ` (${formatBytes(effects.bytesSent!)})` : ''
  const who = decision.agentName?.trim()
  const execs = (effects.notableEvents ?? []).filter((e) => e.args)
  for (const event of [...execs].reverse()) {
    const image = parseImageRef(event.args)
    if (image) return { kind: 'push', text: `pushed ${image.name}:${image.tag} to ${image.host}${image.path ? `/${image.path}` : ''}${size}` }
  }
  for (const event of execs) {
    const put = parsePutPath(event.args)
    const target = event.dest || dest
    if (put && target) {
      const repo = distributionRepo(put)
      return { kind: 'upload', text: `uploaded to ${hostOf(target)}${repo ? `/${repo}` : ''}${size}` }
    }
  }
  const story = credentialStory([decision])
  if (story) return { kind: 'credential', text: who ? `${who} ${story}` : story }
  if (dest && hasBytes(decision)) return { kind: 'send', text: bytes ? `sent ${formatBytes(effects.bytesSent!)} to ${dest}` : `sent data to ${dest}` }
  if (dest) return { kind: 'connect', text: `connected to ${dest}` }
  const binary = effects.binaries?.[0]?.value ?? effects.notableEvents?.find((e) => e.binary)?.binary
  const ran = binary ? binaryName(binary) : decision.action.toolName
  const request = stripSynthetic(decision.action.requestSummary?.trim() ?? '').trim()
  return { kind: 'ran', text: `${who ? `${who} ran` : 'ran'} ${ran}${request ? ` — ${request}` : ''}` }
}

/**
 * An incident's phrase from its most descriptive decision (a push over an
 * upload over the newest), with bytes summed only when every decision has them.
 */
export function incidentPhrase(decisions: DecisionView[]): string {
  let best = decisions[0]
  let bestRank = -1
  for (const d of decisions) {
    const rank = ACTION_RANK[describeAction(d, false).kind]
    if (rank > bestRank) {
      best = d
      bestRank = rank
    }
  }
  const described = describeAction(best, false)
  const all = decisions.every(hasBytes)
  const total = all ? decisions.reduce((sum, d) => sum + (d.action.effects?.bytesSent ?? 0), 0) : 0
  if (!all) return described.text
  if (described.kind === 'send') return described.text.replace(/^sent data to/, `sent ${formatBytes(total)} to`)
  if (described.kind === 'push' || described.kind === 'upload') return `${described.text} (${formatBytes(total)})`
  return described.text
}

/**
 * One plain line per row: "sent 2.6 MB to host:443 — breaks charter rule 1",
 * "scorer unavailable — would hold · summarizer ran python3 — Summarize the log".
 */
export function rowSummary(decision: DecisionView, config: OversightConfig | undefined, phraseOverride?: string): string {
  const phrase = phraseOverride ?? actionPhrase(decision)
  const finding = decision.charterFindings?.[0]
  if (finding) {
    const rule = findCharterRule(config, decision.charterRevision, finding.boundID)
    return `${phrase} — ${rule ? `breaks rule ${rule.bound.number}` : 'breaks an undeclared rule'}`
  }
  if (decision.signal === 'monitor_unavailable' || decision.disposition === 'fail_closed') return `scorer unavailable — would hold · ${phrase}`
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
  taskName: string
  /** The most severe badge's kind, for styling and tests. */
  kind: RowBadgeKind
  badge: RowBadgeKind
  title: string
  /** The broken rules' sentences, shown on hover. */
  detail?: string
  decisions: DecisionView[]
  lastSeen: string
}

/** "rules 4 and 3", "rule 2", "rules 1, 2 and 5", in the order given. */
function ruleList(numbers: number[]): string {
  if (numbers.length === 1) return `rule ${numbers[0]}`
  return `rules ${numbers.slice(0, -1).join(', ')} and ${numbers[numbers.length - 1]}`
}

const CREDENTIAL_WORDS = /credential|secret|token|password|key/i

/**
 * A story across an incident's decisions, oldest first, built from fields:
 * a credential read ("read a token") and a later connection ("then sent it
 * to host"). Undefined when the fields tell no such story.
 */
export function credentialStory(decisions: DecisionView[]): string | undefined {
  const oldestFirst = [...decisions].sort((a, b) => (a.seq ?? 0) - (b.seq ?? 0) || a.observedAt.localeCompare(b.observedAt))
  let readAt = -1
  let what = 'a credential'
  oldestFirst.forEach((d, i) => {
    if (readAt >= 0) return
    const effects = d.action.effects ?? {}
    const counted = (effects.counts?.credential_read ?? 0) > 0
    const tainted = [...(d.action.taintFlags ?? [])].find((t) => CREDENTIAL_WORDS.test(t))
    if (!counted && !tainted) return
    readAt = i
    const hint = [tainted ?? '', ...(effects.paths ?? []).map((p) => p.value)].join(' ')
    if (/token/i.test(hint)) what = 'a token'
  })
  if (readAt < 0) return undefined
  for (const d of oldestFirst.slice(readAt)) {
    const effects = d.action.effects ?? {}
    const connected = (effects.counts?.net_connect ?? 0) > 0 || (effects.destinations?.length ?? 0) > 0
    const dest = effects.destinations?.[0]?.value ?? effects.notableEvents?.find((e) => e.dest)?.dest
    if (connected && dest) return `read ${what}, then sent it to ${hostOf(dest)}`
  }
  return `read ${what}`
}

export interface IncidentText {
  title: string
  /** The broken rules' sentences, for a title attribute. */
  detail?: string
}

/** An incident's one-line title: what happened, then the rules it broke in the order first broken. */
function incidentText(actor: string, decisions: DecisionView[], badge: RowBadgeKind, config: OversightConfig | undefined): IncidentText {
  if (badge !== 'against_charter') return { title: rowSummary(decisions[0], config, incidentPhrase(decisions)) }
  const oldestFirst = [...decisions].sort((a, b) => (a.seq ?? 0) - (b.seq ?? 0) || a.observedAt.localeCompare(b.observedAt))
  const numbers: number[] = []
  const sentences: string[] = []
  let undeclared = false
  for (const d of oldestFirst) {
    for (const f of d.charterFindings ?? []) {
      const rule = findCharterRule(config, d.charterRevision, f.boundID)
      if (rule && !numbers.includes(rule.bound.number)) {
        numbers.push(rule.bound.number)
        sentences.push(`Rule ${rule.bound.number}: ${rule.bound.sentence}`)
      } else if (!rule && !sentences.includes(f.sentence)) {
        undeclared = true
        sentences.push(f.sentence)
      }
    }
  }
  const story = credentialStory(decisions) ?? incidentPhrase(decisions)
  const who = decisions[0].agentName?.trim()
  // actionPhrase already names the agent for "ran" lines.
  const subject = who && story.startsWith(`${who} `) ? story : `${actor} ${story}`
  const rules = [numbers.length > 0 ? ruleList(numbers) : '', undeclared ? 'an undeclared rule' : ''].filter(Boolean).join(' and ')
  return { title: `${subject} — breaks ${rules}`, detail: sentences.join('\n') }
}

/**
 * Groups the decisions that need a person into incidents, one per agent run
 * (agent and Task), carrying the most severe badge among them; most severe
 * first, then most recent.
 */
export function groupIncidents(decisions: DecisionView[], config: OversightConfig | undefined): Incident[] {
  const groups = new Map<string, Incident>()
  const newestFirst = [...decisions].sort((a, b) => (b.seq ?? 0) - (a.seq ?? 0) || b.observedAt.localeCompare(a.observedAt))
  for (const decision of newestFirst) {
    if (!needsHuman(decision)) continue
    const badge = rowBadgeKind(decision)
    const actor = decision.agentName || decision.taskName
    const key = `${actor}\u0000${decision.taskName}`
    const existing = groups.get(key)
    if (existing) {
      existing.decisions.push(decision)
      if (rowBadge(badge).rank > rowBadge(existing.badge).rank) {
        existing.badge = badge
        existing.kind = badge
      }
      continue
    }
    groups.set(key, { key, actor, taskName: decision.taskName, kind: badge, badge, title: '', decisions: [decision], lastSeen: decision.observedAt })
  }
  for (const incident of groups.values()) Object.assign(incident, incidentText(incident.actor, incident.decisions, incident.badge, config))
  return [...groups.values()].sort((a, b) => rowBadge(b.badge).rank - rowBadge(a.badge).rank || b.lastSeen.localeCompare(a.lastSeen))
}

export interface CharterBreaks {
  /** Distinct bounds broken. */
  broken: number
  /** The most-broken declared rule and how many decisions broke it. */
  top?: { number: number; count: number }
}

/** Which rules these decisions broke, by declared rule number. */
export function charterBreaks(decisions: DecisionView[], config: OversightConfig | undefined): CharterBreaks {
  const bounds = new Set<string>()
  const byRule = new Map<number, number>()
  for (const d of decisions) {
    const seen = new Set<number>()
    for (const f of d.charterFindings ?? []) {
      bounds.add(f.boundID)
      const rule = findCharterRule(config, d.charterRevision, f.boundID)
      if (rule && !seen.has(rule.bound.number)) {
        seen.add(rule.bound.number)
        byRule.set(rule.bound.number, (byRule.get(rule.bound.number) ?? 0) + 1)
      }
    }
  }
  let top: CharterBreaks['top']
  for (const [number, count] of byRule) if (!top || count > top.count || (count === top.count && number < top.number)) top = { number, count }
  return { broken: bounds.size, top }
}
