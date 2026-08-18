/**
 * Client-side mirror of the controller's workspace source-ref validation
 * (publisherservice.CanonicalWorkspaceSourceRef via runtimeWorkspaceSourceRef):
 * a selector must be a full lower-case commit SHA, a refs/heads/... branch, a
 * refs/tags/... tag, or a short ref name; other refs/ namespaces and malformed
 * refs are rejected. The controller preflight remains authoritative; this
 * rejects obviously doomed selectors before a Task is created, so it must
 * never be stricter than the controller's rule.
 */

const SHORT_REF_MAX = 1024 - 'refs/heads/'.length

function hasControl(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    if (code < 0x20 || code === 0x7f) return true
  }
  return false
}

function isCanonicalObjectId(value: string): boolean {
  return (
    (value.length === 40 || value.length === 64) && /^[0-9a-f]+$/.test(value) && value.replaceAll('0', '') !== ''
  )
}

function looksLikeObjectId(value: string): boolean {
  return (value.length === 40 || value.length === 64) && /^[0-9a-fA-F]+$/.test(value)
}

function commonRefError(value: string): string | null {
  if (value !== value.trim() || hasControl(value) || value.includes('\\') || !(value.isWellFormed?.() ?? true)) {
    return 'ref is non-canonical'
  }
  return null
}

function refPathError(refPath: string): string | null {
  if (
    refPath.startsWith('-') ||
    refPath.endsWith('/') ||
    refPath.endsWith('.') ||
    refPath.includes('..') ||
    refPath.includes('//') ||
    refPath.includes('@{') ||
    /[ ~^:?*[]/.test(refPath)
  ) {
    return 'ref contains a forbidden sequence'
  }
  for (const component of refPath.split('/')) {
    if (
      !component ||
      component === '.' ||
      component === '..' ||
      component === '@' ||
      component.startsWith('.') ||
      component.endsWith('.lock')
    ) {
      return 'ref contains a forbidden component'
    }
  }
  return null
}

/** Returns an error description for an invalid source ref selector, or null when the controller would accept it. */
export function workspaceSourceRefError(ref: string): string | null {
  if (isCanonicalObjectId(ref)) return null
  if (looksLikeObjectId(ref)) return 'Git object ID must be a lower-case SHA-1 or SHA-256'
  if (ref.startsWith('refs/heads/') || ref.startsWith('refs/tags/')) {
    const prefix = ref.startsWith('refs/heads/') ? 'refs/heads/' : 'refs/tags/'
    if (ref.length <= prefix.length || ref.length > 1024) return 'ref is non-canonical'
    const common = commonRefError(ref)
    if (common) return common
    return refPathError(ref.slice(prefix.length))
  }
  if (ref.startsWith('refs/')) return 'source ref uses an unsupported canonical namespace (only refs/heads/ and refs/tags/ are allowed)'
  if (!ref || ref.length > SHORT_REF_MAX) return 'short source ref is empty or too long'
  const common = commonRefError(ref)
  if (common) return common
  return refPathError(ref)
}

/** Validates a branch selector, defaulting short names into refs/heads/ like the controller does. */
export function workspaceSourceBranchError(branch: string): string | null {
  const candidate = branch.startsWith('refs/') ? branch : `refs/heads/${branch}`
  return workspaceSourceRefError(candidate)
}
