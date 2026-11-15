const loginReturnKey = 'orka-login-return'

/**
 * Remembers the page a signed-out visitor was sent to /login from, so that
 * signing in returns there. Only the path and query are stored: the fragment
 * stays in the address bar, because a connector consent callback carries its
 * one-time completion token there and that must never be persisted.
 */
export function rememberLoginReturn(path: string) {
  try {
    sessionStorage.setItem(loginReturnKey, path)
  } catch {
    // Without storage, signing in lands on the dashboard home.
  }
}

/**
 * Where to go after signing in: the remembered dashboard path with the
 * fragment the address bar still holds (the router's `location.hash`, without
 * the `#`), or the dashboard home. The router's fragment is used rather than
 * `window.location.hash`, which holds the route itself under hash routing.
 */
export function takeLoginReturn(fragment = ''): string {
  let path: string | null = null
  try {
    path = sessionStorage.getItem(loginReturnKey)
    sessionStorage.removeItem(loginReturnKey)
  } catch {
    // Fall through to the dashboard home.
  }
  if (!path || !path.startsWith('/') || path.startsWith('//') || path.startsWith('/\\') || path.startsWith('/login')) {
    return '/'
  }
  return fragment ? `${path}#${fragment}` : path
}

const hashTokenPrefix = '#token='
let pendingHashToken: string | null = null

/**
 * Takes a `#token=...` sign-in fragment (from `orka dashboard` or a deep link)
 * out of the address bar and holds it in memory. Runs once at app start,
 * before the router reads the URL, so the root redirect and the login page
 * never race over the fragment and the token never lands in history.
 */
export function captureHashToken() {
  const { hash, pathname, search } = window.location
  if (!hash.startsWith(hashTokenPrefix)) return
  window.history.replaceState(window.history.state, '', pathname + search)
  const token = hash.slice(hashTokenPrefix.length)
  if (token) pendingHashToken = token
}

/** Whether a captured sign-in token is still waiting for the login page. */
export function hasPendingHashToken(): boolean {
  return pendingHashToken !== null
}

/** Hands the captured sign-in token to the login page, once. */
export function takeHashToken(): string | null {
  const token = pendingHashToken
  pendingHashToken = null
  return token
}
