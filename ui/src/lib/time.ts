export function humanizeDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

export interface TimeAgoOptions {
  empty?: string
  compact?: boolean
}

export function timeAgo(ts?: string, { empty = '-', compact = false }: TimeAgoOptions = {}): string {
  if (!ts) return empty
  const ms = new Date(ts).getTime()
  if (Number.isNaN(ms)) return empty
  const duration = humanizeDuration((Date.now() - ms) / 1000)
  return compact ? duration : `${duration} ago`
}

export interface FormatTimestampOptions {
  empty?: string
  hour12?: boolean
}

export function formatTimestamp(ts?: string, { empty = 'Never', hour12 }: FormatTimestampOptions = {}): string {
  if (!ts) return empty
  const parsed = new Date(ts)
  if (Number.isNaN(parsed.getTime())) return ts
  return parsed.toLocaleString(undefined, hour12 === undefined ? undefined : { hour12 })
}
