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

/** Removes the spent completion token from the address bar. */
export function clearCompletionFragment() {
  if (typeof window === 'undefined') return
  try {
    window.history.replaceState(null, '', window.location.pathname + window.location.search)
  } catch {
    // Leaving the fragment in place is harmless: the token is single use.
  }
}

/** The CLI command that spends a completion token when the page cannot. */
export function completionCommand(name: string, namespace: string | undefined): string {
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
    case 'access_denied':
      return 'You cancelled the consent, so nothing was linked.'
    case undefined:
    case '':
      return 'Linking failed.'
    default:
      return `Linking failed (${reason}).`
  }
}
