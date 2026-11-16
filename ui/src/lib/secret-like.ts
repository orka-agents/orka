// Keys that look credential-bearing, values that look like a credential, and
// annotations known to carry a whole spec (kubectl last-applied embeds env).
export const SECRET_KEY_PATTERN = /token|secret|key|password|cred|auth/i
export const SECRET_VALUE_PATTERN = /(?:sk-[A-Za-z0-9_-]{6,}|github_pat_|gh[pousr]_[A-Za-z0-9_]{6,}|xox[baprs]-|AIza[0-9A-Za-z_-]{10,}|AKIA[0-9A-Z]{8,}|Bearer\s+\S+|Basic\s+\S+|(?:api[_-]?key|token|secret|password|credential)["']?\s*[:=]\s*\S+|-----BEGIN [A-Z ]*PRIVATE KEY-----|hooks\.slack\.com(?::\d+)?\/(?:services|workflows|triggers)\/|discord(?:app)?\.com(?::\d+)?\/api\/(?:v\d+\/)?webhooks\/)/i
// A Slack or Discord webhook URL, whose path is the secret.
export const WEBHOOK_URL = /https?:\/\/(?:[\w-]+\.)*(?:hooks\.slack\.com(?::\d+)?\/(?:services|workflows|triggers)\/|discord(?:app)?\.com(?::\d+)?\/api\/(?:v\d+\/)?webhooks\/)/i

// Signs that free text holds a credential. Text that shows any of them is
// hidden whole: finding where a secret ends inside prose (quoting, escapes,
// multi-parameter headers) is not reliable, and a partial cut leaves pieces.
const CREDENTIAL_SIGNS: RegExp[] = [
  // Provider token shapes (OpenAI/Anthropic, GitHub, GitLab, Slack, Google, AWS,
  // npm, PyPI, Hugging Face, Stripe, SendGrid, DigitalOcean, Shopify, Linear)
  // and PEM keys, case-sensitive.
  /\b(?:sk-[A-Za-z0-9_-]{6,}|github_pat_\w+|gh[pousr]_[A-Za-z0-9_]{6,}|gl(?:pat|rt|dt|oas|ptt)-[\w-]{10,}|xox[abeprs]-[\w-]+|xapp-[\w-]+|AIza[0-9A-Za-z_-]{10,}|(?:AKIA|ASIA)[0-9A-Z]{8,}|npm_[A-Za-z0-9]{20,}|pypi-[\w-]{20,}|hf_[A-Za-z0-9]{20,}|[sr]k_(?:live|test)_[A-Za-z0-9]{10,}|SG\.[\w-]{16,}\.[\w-]{16,}|do[por]_v1_[a-f0-9]{20,}|shp(?:at|ss|ca|pa)_[a-fA-F0-9]{20,}|lin_(?:api|oauth)_[A-Za-z0-9]{20,})|-----BEGIN [A-Z ]*PRIVATE KEY-----/,
  // A Bearer token in any case; the length keeps "Bearer tokens expire" out.
  /\bbearer\s+[A-Za-z0-9._~+/-]{16,}/i,
  // An Authorization header, in any case, also as a (possibly escaped) JSON key.
  /\b(?:proxy-)?authorization(?:\\?["'])?\s*:/i,
  // `password = value` and the like: the name only has to end right before the
  // separator, so prefixed names (access_token, clientSecret), "API key:" and
  // (escaped) JSON keys count, and `max_tokens: 100` does not. No leading
  // wildcard, so matching stays linear in the text's length.
  /(?:(?:api|access|secret|private|signing|encryption)[\s_-]?key|token|secret|password|passwd|pwd|credential)(?:\\?["'])?\s*[:=]\s*\S/i,
  // Userinfo in a URL. The scheme is bounded so a long dotted run cannot make
  // matching quadratic.
  /\b[a-z][a-z\d+.-]{0,31}:\/\/[^\s/?#"<>]*@/i,
  WEBHOOK_URL,
]

// A signed URL's query parameter, its name checked segment by segment (sig,
// X-Amz-Signature, access_token) so "design" or "assignees" do not count. Two
// linear steps, not one pattern with wildcards on both sides of the name.
const QUERY_PARAMETER = /[?&]([\w.-]{1,128})=/g
const CREDENTIAL_PARAMETER = /(?:^|[_.-])(?:sig|signature|token|key|secret|credential|password)(?:$|[_.-])/i

function hasCredentialParameter(text: string): boolean {
  for (const [, name] of text.matchAll(QUERY_PARAMETER)) {
    if (CREDENTIAL_PARAMETER.test(name)) return true
  }
  return false
}

// HTTP Basic credentials without their header: base64 that decodes to
// `user:password`. "Basic Authentication" decodes to nothing readable.
const BASIC_CREDENTIALS = /\bBasic\s+([A-Za-z0-9+/]{8,}={0,2})(?![\w+/=])/gi

function hasBasicCredentials(text: string): boolean {
  for (const [, encoded] of text.matchAll(BASIC_CREDENTIALS)) {
    try {
      if (/^[\x20-\x7e]+:[\x20-\x7e]*$/.test(atob(encoded))) return true
    } catch {
      // Not base64.
    }
  }
  return false
}

/** True when free text shows a sign of holding a credential. */
export const containsCredential = (text: string) =>
  CREDENTIAL_SIGNS.some((sign) => sign.test(text)) || hasCredentialParameter(text) || hasBasicCredentials(text)

export const UNSAFE_KEY_PATTERN = /last-applied-configuration|kubectl\.kubernetes\.io/i

// Values long enough to plausibly carry an embedded blob or token.
export const MAX_VALUE_CHARS = 80

/** True for a label or annotation the dashboard should not show the value of. */
export const looksSecret = (key: string, value: string) =>
  SECRET_KEY_PATTERN.test(key) || SECRET_VALUE_PATTERN.test(value) || value.length > MAX_VALUE_CHARS
