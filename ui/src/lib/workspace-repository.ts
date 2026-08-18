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
  const path = parsed.pathname
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
