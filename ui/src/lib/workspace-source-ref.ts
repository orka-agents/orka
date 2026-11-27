
import { isWellFormedText } from './workspace-repository'

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
    (value.length === 40 || value.length === 64) && /^[0-9a-f]+$/.test(value) && value.replace(/0/g, '') !== ''
  )
}

function looksLikeObjectId(value: string): boolean {
  return (value.length === 40 || value.length === 64) && /^[0-9a-fA-F]+$/.test(value)
}

function commonRefError(value: string): string | null {
  if (value !== value.trim() || hasControl(value) || value.includes('\\') || !isWellFormedText(value)) {
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

export function workspaceSourceBranchError(branch: string): string | null {
  const candidate = branch.startsWith('refs/') ? branch : `refs/heads/${branch}`
  return workspaceSourceRefError(candidate)
}

export function workspacePublicationBranchError(branch: string): string | null {
  const ref = branch.startsWith('refs/heads/') ? branch : `refs/heads/${branch}`
  if (ref !== ref.trim() || hasControl(ref) || !isWellFormedText(ref)) {
    return 'branch is non-canonical'
  }
  if (ref.length > 1024) return 'branch is too long'
  if (ref.length === 'refs/heads/'.length) return 'branch is required'
  if (
    ref.includes('..') ||
    ref.includes('@{') ||
    /[ ~^:?*[\\]/.test(ref) ||
    ref.endsWith('.') ||
    ref.endsWith('/') ||
    ref.includes('//')
  ) {
    return 'branch is not a canonical Git branch name'
  }
  return null
}
