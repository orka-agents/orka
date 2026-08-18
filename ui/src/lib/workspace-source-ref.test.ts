import { describe, expect, it } from 'vitest'
import { workspaceSourceBranchError, workspaceSourceRefError } from './workspace-source-ref'

describe('workspaceSourceRefError', () => {
  it.each([
    ['full lower-case SHA-1', 'a'.repeat(40)],
    ['full lower-case SHA-256', 'ab'.repeat(32)],
    ['canonical branch ref', 'refs/heads/main'],
    ['canonical tag ref', 'refs/tags/v1.2.3'],
    ['short ref name', 'feature/my-branch'],
  ])('accepts %s', (_name, ref) => {
    expect(workspaceSourceRefError(ref)).toBeNull()
  })

  it.each([
    ['unsupported refs namespace', 'refs/remotes/origin/main'],
    ['refs/ root', 'refs/'],
    ['upper-case object ID', 'A'.repeat(40)],
    ['double dots', 'refs/heads/bad..ref'],
    ['trailing slash', 'refs/heads/bad/'],
    ['trailing dot', 'refs/heads/bad.'],
    ['space', 'refs/heads/bad ref'],
    ['tilde', 'refs/heads/bad~ref'],
    ['lock suffix', 'refs/heads/bad.lock'],
    ['hidden component', 'refs/heads/.hidden/ref'],
    ['at-brace sequence', 'refs/heads/bad@{ref'],
    ['backslash', 'refs/heads/bad\\ref'],
    ['empty component', 'refs/heads/a//b'],
  ])('rejects %s', (_name, ref) => {
    expect(workspaceSourceRefError(ref)).not.toBeNull()
  })
})

describe('workspaceSourceBranchError', () => {
  it('defaults short names into refs/heads/ like the controller', () => {
    expect(workspaceSourceBranchError('main')).toBeNull()
    expect(workspaceSourceBranchError('bad..branch')).not.toBeNull()
  })

  it('rejects branch selectors in unsupported namespaces', () => {
    expect(workspaceSourceBranchError('refs/remotes/origin/main')).not.toBeNull()
  })
})
