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
 * fragment the address bar still holds, or the dashboard home.
 */
export function takeLoginReturn(): string {
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
  return path + window.location.hash
}
