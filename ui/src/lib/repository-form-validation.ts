const GITHUB_PATH_SEGMENT = /^[A-Za-z0-9._-]+(?:\.git)?$/

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

const EVERY_DESCRIPTOR = /^@every(?: (.*))?$/
const GO_DURATION = /^[-+]?(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$|^[-+]?0$/
const GO_DURATION_PART = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g
const GO_DURATION_UNIT_SECONDS: Record<string, number> = {
  ns: 1e-9, us: 1e-6, 'µs': 1e-6, 'μs': 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600,
}
const GO_DURATION_MAX_SECONDS = 2 ** 63 / 1e9
const MIN_EVERY_SECONDS = 60

function goDurationSeconds(value: string) {
  if (!GO_DURATION.test(value)) return undefined
  let seconds = 0
  for (const [, amount, unit] of value.matchAll(GO_DURATION_PART)) {
    seconds += Number(amount) * GO_DURATION_UNIT_SECONDS[unit]
  }
  if (seconds > GO_DURATION_MAX_SECONDS) return undefined
  return value.startsWith('-') ? -seconds : seconds
}

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
  names?: readonly string[]
}

const CRON_FIELDS: readonly CronField[] = [
  { min: 0, max: 59 },
  { min: 0, max: 23 },
  { min: 1, max: 31 },
  { min: 1, max: 12, names: ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'] },
  { min: 0, max: 6, names: ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'] },
]

function cronValue(token: string, field: CronField) {
  if (/^\d+$/.test(token)) return Number(token)
  const index = field.names?.indexOf(token.toLowerCase()) ?? -1
  return index < 0 ? undefined : field.min + index
}

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

export function isCronSchedule(value: string) {
  const schedule = stripCronTimeZone(value)
  if (CRON_DESCRIPTOR.test(schedule)) return true
  if (EVERY_DESCRIPTOR.test(schedule)) return everyScheduleError(schedule) === undefined
  const fields = schedule.split(/\s+/)
  return fields.length === CRON_FIELDS.length &&
    fields.every((field, index) => field.split(',').every((item) => isCronFieldItem(item, CRON_FIELDS[index])))
}
