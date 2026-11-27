
const GITHUB_SAFE_SEGMENT = /^[A-Za-z0-9._-]+$/

const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/

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

function isForbiddenRepositoryIpLiteral(hostname: string): boolean {
  const host = hostname.replace(/^\[|\]$/g, '').toLowerCase()
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(host)
  if (v4) {
    const [a, b, c] = [Number(v4[1]), Number(v4[2]), Number(v4[3])]
    if (a === 127) return true
    if (host === '0.0.0.0') return true
    if (a === 169 && b === 254) return true
    if (a === 224 && b === 0 && c === 0) return true
    return false
  }
  if (host.includes(':')) {
    if (host === '::1' || host === '::') return true
    if (/^fe[89ab]/.test(host)) return true
    if (/^ff[0-9a-f]2:/.test(host)) return true
    return false
  }
  return false
}

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
