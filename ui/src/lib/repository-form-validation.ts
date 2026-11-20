const GITHUB_PATH_SEGMENT = /^[A-Za-z0-9._-]+(?:\.git)?$/

/**
 * Accepts only a credential-free GitHub repository root: `https://github.com/org/repo`
 * (optionally `.git`) or `git@github.com:org/repo`. Pull request, issue, branch,
 * file, query, fragment, plain-HTTP, other-host, and token-bearing URLs are rejected.
 */
export function isCredentialFreeGitHubRepositoryURL(value: string) {
  const repoURL = value.trim()
  if (!repoURL) return false

  if (/^git@github\.com:[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+(?:\.git)?$/.test(repoURL)) {
    return true
  }

  if (!repoURL.startsWith('https://')) {
    return false
  }

  try {
    const parsed = new URL(repoURL)
    if (parsed.protocol !== 'https:' || parsed.hostname !== 'github.com') return false
    if (parsed.username || parsed.password || parsed.search || parsed.hash) return false

    const pathParts = parsed.pathname.split('/').filter(Boolean)
    if (pathParts.length !== 2) return false

    return pathParts.every((part) => GITHUB_PATH_SEGMENT.test(part))
  } catch {
    return false
  }
}

const CRON_DESCRIPTOR = /^@(?:yearly|annually|monthly|weekly|daily|midnight|hourly)$/

// robfig/cron reads everything after exactly "@every " as a Go duration.
const EVERY_DESCRIPTOR = /^@every(?: (.*))?$/
const GO_DURATION = /^[-+]?(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$|^[-+]?0$/
const GO_DURATION_PART = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g
const GO_DURATION_UNIT_SECONDS: Record<string, number> = {
  ns: 1e-9, us: 1e-6, 'µs': 1e-6, 'μs': 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600,
}
// time.ParseDuration refuses anything past int64 nanoseconds (about 292 years).
const GO_DURATION_MAX_SECONDS = 2 ** 63 / 1e9
const MIN_EVERY_SECONDS = 60

/** The seconds a Go duration string stands for, or undefined when Go would not parse it. */
function goDurationSeconds(value: string) {
  if (!GO_DURATION.test(value)) return undefined
  let seconds = 0
  for (const [, amount, unit] of value.matchAll(GO_DURATION_PART)) {
    seconds += Number(amount) * GO_DURATION_UNIT_SECONDS[unit]
  }
  if (seconds > GO_DURATION_MAX_SECONDS) return undefined
  return value.startsWith('-') ? -seconds : seconds
}

/**
 * Why an `@every` schedule is refused, or undefined when the value is not an
 * `@every` schedule or its interval is accepted. Like the controller, it
 * needs a Go duration (`5m`, `1h30m`) of at least one minute: shorter, zero,
 * and negative intervals would start a new scan almost continuously.
 */
export function everyScheduleError(value: string) {
  const match = EVERY_DESCRIPTOR.exec(stripCronTimeZone(value))
  if (!match) return undefined
  const seconds = goDurationSeconds(match[1] ?? '')
  if (seconds === undefined) {
    return `Schedule "${value.trim()}" needs a Go duration after @every, such as @every 5m or @every 1h30m.`
  }
  if (seconds < MIN_EVERY_SECONDS) {
    return `Schedule "${value.trim()}" needs an interval of at least 1m, such as @every 5m or @every 1h30m.`
  }
  return undefined
}

function stripCronTimeZone(value: string) {
  return value.trim().replace(/^(?:CRON_)?TZ=\S+\s+/, '')
}

interface CronField {
  min: number
  max: number
  /** Case-insensitive names; the first one stands for `min`. */
  names?: readonly string[]
}

// Bounds and names of the controller's standard cron parser (robfig/cron
// ParseStandard), which rejects anything outside them, including weekday 7.
const CRON_FIELDS: readonly CronField[] = [
  { min: 0, max: 59 }, // minute
  { min: 0, max: 23 }, // hour
  { min: 1, max: 31 }, // day of month
  { min: 1, max: 12, names: ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'] },
  { min: 0, max: 6, names: ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'] }, // day of week
]

function cronValue(token: string, field: CronField) {
  if (/^\d+$/.test(token)) return Number(token)
  const index = field.names?.indexOf(token.toLowerCase()) ?? -1
  return index < 0 ? undefined : field.min + index
}

/** One comma-separated item: `*` or `?`, a value, or a range, each optionally `/step`. */
function isCronFieldItem(item: string, field: CronField) {
  const [range, step, ...extraSteps] = item.split('/')
  if (extraSteps.length > 0) return false
  if (step !== undefined && !(/^\d+$/.test(step) && Number(step) > 0)) return false
  if (range === '*' || range === '?') return true
  const [low, high, ...extraBounds] = range.split('-')
  if (extraBounds.length > 0) return false
  const start = cronValue(low, field)
  const end = high === undefined ? start : cronValue(high, field)
  return start !== undefined && end !== undefined && field.min <= start && start <= end && end <= field.max
}

/**
 * Checks a cron schedule the way the controller parses it: five
 * space-separated fields (minute 0-59, hour 0-23, day of month 1-31, month
 * 1-12 or JAN-DEC, day of week 0-6 or SUN-SAT) built from `*`, `?`, values,
 * ranges, steps, and lists, one of the @-descriptors, or `@every` with a Go
 * duration of at least one minute, optionally after a `CRON_TZ=`/`TZ=`
 * prefix. Empty list items are refused as typos. The time zone stays the
 * server's call.
 */
export function isCronSchedule(value: string) {
  const schedule = stripCronTimeZone(value)
  if (CRON_DESCRIPTOR.test(schedule)) return true
  if (EVERY_DESCRIPTOR.test(schedule)) return everyScheduleError(schedule) === undefined
  const fields = schedule.split(/\s+/)
  return fields.length === CRON_FIELDS.length &&
    fields.every((field, index) => field.split(',').every((item) => isCronFieldItem(item, CRON_FIELDS[index])))
}
