// Keys that look credential-bearing, values that look like a credential, and
// annotations known to carry a whole spec (kubectl last-applied embeds env).
export const SECRET_KEY_PATTERN = /token|secret|key|password|cred|auth/i
export const SECRET_VALUE_PATTERN = /(?:sk-[A-Za-z0-9_-]{6,}|github_pat_|gh[pousr]_[A-Za-z0-9_]{6,}|xox[baprs]-|AIza[0-9A-Za-z_-]{10,}|AKIA[0-9A-Z]{8,}|Bearer\s+\S+|Basic\s+\S+|(?:api[_-]?key|token|secret|password|credential)["']?\s*[:=]\s*\S+|-----BEGIN [A-Z ]*PRIVATE KEY-----)/i
export const UNSAFE_KEY_PATTERN = /last-applied-configuration|kubectl\.kubernetes\.io/i

// Values long enough to plausibly carry an embedded blob or token.
export const MAX_VALUE_CHARS = 80

/** True for a label or annotation the dashboard should not show the value of. */
export const looksSecret = (key: string, value: string) =>
  SECRET_KEY_PATTERN.test(key) || SECRET_VALUE_PATTERN.test(value) || value.length > MAX_VALUE_CHARS
