export const SECRET_KEY_PATTERN = /token|secret|key|password|cred|auth/i
export const SECRET_VALUE_PATTERN = /(?:sk-[A-Za-z0-9_-]{6,}|github_pat_|gh[pousr]_[A-Za-z0-9_]{6,}|xox[baprs]-|AIza[0-9A-Za-z_-]{10,}|AKIA[0-9A-Z]{8,}|Bearer\s+\S+|Basic\s+\S+|(?:api[_-]?key|token|secret|password|credential)["']?\s*[:=]\s*\S+|-----BEGIN [A-Z ]*PRIVATE KEY-----|hooks\.slack\.com(?::\d+)?\/(?:services|workflows|triggers)\/|discord(?:app)?\.com(?::\d+)?\/api\/(?:v\d+\/)?webhooks\/)/i
export const WEBHOOK_URL = /https?:\/\/(?:[\w-]+\.)*(?:hooks\.slack\.com(?::\d+)?\/(?:services|workflows|triggers)\/|discord(?:app)?\.com(?::\d+)?\/api\/(?:v\d+\/)?webhooks\/)/i

const CREDENTIAL_SIGNS: RegExp[] = [
  /\b(?:sk-[A-Za-z0-9_-]{6,}|github_pat_\w+|gh[pousr]_[A-Za-z0-9_]{6,}|gl(?:pat|rt|dt|oas|ptt)-[\w-]{10,}|xox[abeprs]-[\w-]+|xapp-[\w-]+|AIza[0-9A-Za-z_-]{10,}|(?:AKIA|ASIA)[0-9A-Z]{8,}|npm_[A-Za-z0-9]{20,}|pypi-[\w-]{20,}|hf_[A-Za-z0-9]{20,}|[sr]k_(?:live|test)_[A-Za-z0-9]{10,}|SG\.[\w-]{16,}\.[\w-]{16,}|do[por]_v1_[a-f0-9]{20,}|shp(?:at|ss|ca|pa)_[a-fA-F0-9]{20,}|lin_(?:api|oauth)_[A-Za-z0-9]{20,})|-----BEGIN [A-Z ]*PRIVATE KEY-----/,
  /\bbearer\s+[A-Za-z0-9._~+/-]{16,}/i,
  /\b(?:proxy-)?authorization(?:\\?["'])?\s*:/i,
  /(?:(?:api|access|secret|private|signing|encryption)[\s_-]?key|token|secret|password|passwd|pwd|credential)(?:\\?["'])?\s*[:=]\s*\S/i,
  /\b[a-z][a-z\d+.-]{0,31}:\/\/[^\s/?#"<>]*@/i,
  WEBHOOK_URL,
]

const QUERY_PARAMETER = /[?&]([\w.-]{1,128})=/g
const CREDENTIAL_PARAMETER = /(?:^|[_.-])(?:sig|signature|token|key|secret|credential|password)(?:$|[_.-])/i

function hasCredentialParameter(text: string): boolean {
  for (const [, name] of text.matchAll(QUERY_PARAMETER)) {
    if (CREDENTIAL_PARAMETER.test(name)) return true
  }
  return false
}

const BASIC_CREDENTIALS = /\bBasic\s+([A-Za-z0-9+/]{8,}={0,2})(?![\w+/=])/gi

function hasBasicCredentials(text: string): boolean {
  for (const [, encoded] of text.matchAll(BASIC_CREDENTIALS)) {
    try {
      if (/^[\x20-\x7e]+:[\x20-\x7e]*$/.test(atob(encoded))) return true
    } catch {
    }
  }
  return false
}

export const containsCredential = (text: string) =>
  CREDENTIAL_SIGNS.some((sign) => sign.test(text)) || hasCredentialParameter(text) || hasBasicCredentials(text)

export const UNSAFE_KEY_PATTERN = /last-applied-configuration|kubectl\.kubernetes\.io/i

export const MAX_VALUE_CHARS = 80

export const looksSecret = (key: string, value: string) =>
  SECRET_KEY_PATTERN.test(key) || SECRET_VALUE_PATTERN.test(value) || value.length > MAX_VALUE_CHARS
