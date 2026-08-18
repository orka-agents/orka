/**
 * Client-side mirror of the controller's workspace repository canonicalization
 * (security.CanonicalRepositoryCloneURL + canonicalWorkspaceRepositoryURL):
 * GitHub SSH roots (git@github.com:owner/repo[.git]) and HTTPS GitHub forms
 * are rewritten to https://github.com/owner/repo, and only credential-free
 * HTTPS URLs without query or fragment are accepted. The controller preflight
 * remains authoritative; this rejects obviously doomed URLs before a Task is
 * created, so it must never be stricter than the controller's rule.
 */

const GITHUB_SAFE_SEGMENT = /^[A-Za-z0-9._-]+$/

// Matches lone UTF-16 surrogates, i.e. strings that cannot encode to valid
// UTF-8 (the equivalent of Go's utf8.ValidString check). Implemented as a
// regex so it does not require the ES2024 String.isWellFormed lib.
const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/

/** True when the string contains no lone surrogates and therefore encodes to valid UTF-8. */
export function isWellFormedText(value: string): boolean {
  return !LONE_SURROGATE.test(value)
}

function githubOwnerRepoFromPath(repoPath: string): [string, string] | null {
  const segments = repoPath
    .replace(/^\/+|\/+$/g, '')
    .replace(/\.git$/, '')
    .split('/')
  if (segments.length !== 2) return null
  for (const segment of segments) {
    if (!segment || segment === '.' || segment === '..' || !GITHUB_SAFE_SEGMENT.test(segment)) return null
  }
  return [segments[0], segments[1]]
}

/** Rewrite GitHub URLs to the canonical credential-free HTTPS clone URL; other URLs are returned trimmed and unchanged. */
export function canonicalRepositoryCloneUrl(raw: string): string {
  const trimmed = raw.trim()
  if (trimmed.startsWith('git@')) {
    const rest = trimmed.slice('git@'.length)
    const colon = rest.indexOf(':')
    if (colon > 0 && rest.slice(0, colon).toLowerCase() === 'github.com') {
      const ownerRepo = githubOwnerRepoFromPath(rest.slice(colon + 1))
      if (ownerRepo) return `https://github.com/${ownerRepo[0]}/${ownerRepo[1]}`
    }
    return trimmed
  }
  let parsed: URL
  try {
    parsed = new URL(trimmed)
  } catch {
    return trimmed
  }
  if (
    parsed.username ||
    parsed.password ||
    parsed.protocol !== 'https:' ||
    parsed.hostname.toLowerCase() !== 'github.com' ||
    parsed.search ||
    parsed.hash
  ) {
    return trimmed
  }
  const ownerRepo = githubOwnerRepoFromPath(parsed.pathname)
  return ownerRepo ? `https://github.com/${ownerRepo[0]}/${ownerRepo[1]}` : trimmed
}

export type WorkspaceRepositoryUrlResult = { url: string } | { error: string }

/**
 * Mirrors the controller/Publisher IP-literal rule: loopback, unspecified,
 * and link-local (unicast or multicast) repository hosts are rejected. The
 * URL parser has already normalized IPv4 shorthand and IPv6 spellings.
 */
function isForbiddenRepositoryIpLiteral(hostname: string): boolean {
  const host = hostname.replace(/^\[|\]$/g, '').toLowerCase()
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(host)
  if (v4) {
    const [a, b, c] = [Number(v4[1]), Number(v4[2]), Number(v4[3])]
    if (a === 127) return true // loopback
    if (host === '0.0.0.0') return true // unspecified
    if (a === 169 && b === 254) return true // link-local unicast
    if (a === 224 && b === 0 && c === 0) return true // link-local multicast
    return false
  }
  if (host.includes(':')) {
    if (host === '::1' || host === '::') return true // loopback / unspecified
    if (/^fe[89ab]/.test(host)) return true // link-local unicast fe80::/10
    if (/^ff[0-9a-f]2:/.test(host)) return true // link-local multicast ffX2::/16
    return false
  }
  return false
}

/**
 * Derives the canonical repository identity for a clone URL that already
 * passed validateWorkspaceRepositoryUrl, mirroring the controller's
 * canonicalWorkspaceRepositoryURL derivation: lower-cased host plus the path
 * with any .git suffix removed, additionally lower-cased for github.com.
 */
export function workspaceRepositoryIdentity(canonicalUrl: string): string | null {
  let parsed: URL
  try {
    parsed = new URL(canonicalUrl)
  } catch {
    return null
  }
  const host = parsed.hostname.toLowerCase()
  let identityPath = parsed.pathname.replace(/^\/+/, '').replace(/\.git$/, '')
  if (!host || !identityPath) return null
  if (host === 'github.com') identityPath = identityPath.toLowerCase()
  return `${host}/${identityPath}`
}

/**
 * Mirrors the harness-v2 workspace relative-root validation so an unsafe
 * subpath is rejected before the Task is created instead of failing
 * RuntimeSession creation. Returns an error description, or null when valid.
 */
export function workspaceSubPathError(subPath: string): string | null {
  const root = subPath.trim()
  if (!root || root === '.') return null
  if (!isWellFormedText(root)) return 'contains invalid characters'
  if (new TextEncoder().encode(root).length > 1024) return 'exceeds 1024 bytes'
  if (root.startsWith('/') || root.includes('\\')) return 'must be a relative slash-separated path'
  for (const segment of root.split('/')) {
    if (!segment || segment === '.' || segment === '..') return 'contains an unsafe segment'
  }
  return null
}

/** Mirrors the controller's identity comparison: exact match, or case-insensitive for github.com identities. */
export function sameWorkspaceRepositoryIdentity(first: string, second: string): boolean {
  const a = first.trim()
  const b = second.trim()
  if (a === b) return true
  return (
    a.toLowerCase().startsWith('github.com/') &&
    b.toLowerCase().startsWith('github.com/') &&
    a.toLowerCase() === b.toLowerCase()
  )
}

/** Canonicalize and validate a workspace repository URL field. Empty input is allowed and returns an empty URL. */
export function validateWorkspaceRepositoryUrl(label: string, raw: string): WorkspaceRepositoryUrlResult {
  const trimmed = raw.trim()
  if (!trimmed) return { url: '' }
  const invalid = (detail: string): WorkspaceRepositoryUrlResult => ({
    error: `${label} ${detail}. Use a credential-free HTTPS URL such as https://github.com/owner/repo (GitHub SSH roots like git@github.com:owner/repo are converted automatically)`,
  })
  const canonical = canonicalRepositoryCloneUrl(trimmed)
  let parsed: URL
  try {
    parsed = new URL(canonical)
  } catch {
    return invalid('must be a credential-free HTTPS URL without query or fragment')
  }
  if (
    parsed.username ||
    parsed.password ||
    parsed.protocol !== 'https:' ||
    !parsed.hostname ||
    parsed.search ||
    parsed.hash
  ) {
    return invalid('must be a credential-free HTTPS URL without query or fragment')
  }
  if (parsed.port && parsed.port !== '443') {
    return invalid('must use the default HTTPS port')
  }
  if (isForbiddenRepositoryIpLiteral(parsed.hostname)) {
    return invalid('uses a forbidden IP literal')
  }
  const path = parsed.pathname
  // The browser URL parser resolves dot segments (/org/../repo -> /repo)
  // before the checks below run, but the original string is what gets
  // submitted and the controller rejects unclean paths. Require the path as
  // written to match the parsed pathname so normalized-away segments are
  // rejected instead of silently accepted.
  const pathStart = canonical.indexOf('/', canonical.indexOf('://') + 3)
  const rawPath = pathStart === -1 ? '' : canonical.slice(pathStart)
  if (rawPath !== path) {
    return invalid('path is invalid')
  }
  if (path === '/' || path.endsWith('/') || path.includes('//')) {
    return invalid('path is invalid')
  }
  const segments = path.slice(1).split('/')
  if (segments.some((segment) => !segment || segment === '.' || segment === '..')) {
    return invalid('path is invalid')
  }
  if (!path.slice(1).replace(/\.git$/, '')) {
    return invalid('path is invalid')
  }
  // Mirror the controller's escaped-path canonicality check: the path as
  // written must round-trip through decode + canonical re-encoding, so
  // escapes like %2F that hide extra path structure are rejected.
  let decodedPath: string
  try {
    decodedPath = decodeURIComponent(path)
  } catch {
    return invalid('has a non-canonical escaped path')
  }
  if (encodeURI(decodedPath) !== path) {
    return invalid('has a non-canonical escaped path')
  }
  return { url: canonical }
}
