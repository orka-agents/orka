/** Types and helpers for the Settings › Connectors page. */

export interface ConnectorTool {
  name: string
  class: 'read' | 'write' | string
}

export interface ConnectorProvider {
  name: string
  namespace: string
  displayName: string
  ready: boolean
  scopes?: { read?: string[]; write?: string[] }
  tools: ConnectorTool[]
}

export type ConnectionMode = 'readOnly' | 'readWrite'

export interface Connection {
  name: string
  namespace: string
  provider: string
  mode: ConnectionMode | string
  state: string
  grantedScopes?: string[]
  linkedAt?: string
  expiresAt?: string
  lastRefreshTime?: string
  ready: boolean
  message?: string
  /** The disconnect is still finishing (tokens are revoked first). */
  deleting?: boolean
}

export interface ConnectionAuthorizeResponse {
  connection: Connection
  authorizeURL?: string
}

/** Query parameters the OAuth callback sends the browser back with. */
export interface ConnectorCallbackSearch {
  status?: 'pending' | 'error' | string
  reason?: string
  connection?: string
  /** The namespace the consent was sealed in; completion must go there. */
  namespace?: string
}

/**
 * The one-time completion token travels in the URL fragment so it never
 * reaches a server log or referrer. It stays in the address bar until the
 * completion succeeds, so a failed attempt (network, expired sign-in) can be
 * retried with a reload; the server accepts the token exactly once.
 */
export function readCompletionToken(): string | null {
  if (typeof window === 'undefined') return null
  const fragment = window.location.hash.startsWith('#') ? window.location.hash.slice(1) : window.location.hash
  if (!fragment) return null
  return new URLSearchParams(fragment).get('completion')
}

/** The query keys the callback redirect sets on this page. */
const callbackSearchKeys = ['status', 'reason', 'connection', 'namespace']

/**
 * Removes a finished consent from the address bar: the spent completion
 * token and the callback's query keys, so a reload or a bookmark of the
 * URL does not look like a consent that still needs finishing.
 */
export function clearConsentCallback() {
  if (typeof window === 'undefined') return
  try {
    const search = new URLSearchParams(window.location.search)
    for (const key of callbackSearchKeys) search.delete(key)
    const rest = search.toString()
    window.history.replaceState(null, '', window.location.pathname + (rest ? `?${rest}` : ''))
  } catch {
    // Leaving the URL in place is harmless: the token is single use.
  }
}

const dns1123Subdomain = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/
const dns1123Label = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

/** Whether a callback-supplied value is a Kubernetes object name. */
export function isKubernetesName(value: string | undefined): value is string {
  return typeof value === 'string' && value.length <= 253 && dns1123Subdomain.test(value)
}

/** Whether a callback-supplied value is a Kubernetes namespace name. */
export function isKubernetesNamespace(value: string | undefined): value is string {
  return typeof value === 'string' && value.length <= 63 && dns1123Label.test(value)
}

/**
 * The CLI command that spends a completion token when the page cannot, or
 * null when the callback's connection or namespace is not a Kubernetes
 * name: those values come from the address bar, and a crafted link must
 * not turn into a command the person is told to paste into a shell.
 */
export function completionCommand(name: string | undefined, namespace: string | undefined): string | null {
  if (!isKubernetesName(name)) return null
  if (namespace !== undefined && namespace !== '' && !isKubernetesNamespace(namespace)) return null
  return `orka connection complete ${name}${namespace ? ` --namespace ${namespace}` : ''} --completion <value from the address bar>`
}

/** Sends the browser to the provider's consent page. */
export function openAuthorizeURL(url: string) {
  window.location.assign(url)
}

/** Human wording for the callback's fixed error reason codes. */
export function callbackReasonMessage(reason: string | undefined): string {
  switch (reason) {
    case 'scopes_denied':
      return 'The provider granted fewer permissions than this link needs. Try again and accept every requested permission.'
    case 'provider_changed':
      return 'The provider configuration changed while you were consenting. Start the link again.'
    case 'mode_changed':
      return 'The link was switched between read only and read and write while you were consenting. Start the link again.'
    case 'consent_superseded':
      return 'A newer consent for this link was already finished, so this older one was not used.'
    case 'access_denied':
      return 'You cancelled the consent, so nothing was linked.'
    case undefined:
    case '':
      return 'Linking failed.'
    default:
      return `Linking failed (${reason}).`
  }
}
