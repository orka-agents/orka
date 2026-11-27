const loginReturnKey = 'orka-login-return'

export function rememberLoginReturn(path: string) {
  try {
    sessionStorage.setItem(loginReturnKey, path)
  } catch {
  }
}

export function takeLoginReturn(fragment = ''): string {
  let path: string | null = null
  try {
    path = sessionStorage.getItem(loginReturnKey)
    sessionStorage.removeItem(loginReturnKey)
  } catch {
  }
  if (!path || !path.startsWith('/') || path.startsWith('//') || path.startsWith('/\\') || path.startsWith('/login')) {
    return '/'
  }
  return fragment ? `${path}#${fragment}` : path
}

const hashTokenPrefix = '#token='
let pendingHashToken: string | null = null

export function captureHashToken() {
  const { hash, pathname, search } = window.location
  if (!hash.startsWith(hashTokenPrefix)) return
  window.history.replaceState(window.history.state, '', pathname + search)
  const token = hash.slice(hashTokenPrefix.length)
  if (token) pendingHashToken = token
}

export function hasPendingHashToken(): boolean {
  return pendingHashToken !== null
}

export function takeHashToken(): string | null {
  const token = pendingHashToken
  pendingHashToken = null
  return token
}
