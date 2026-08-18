import { describe, expect, it } from 'vitest'
import { canonicalRepositoryCloneUrl, validateWorkspaceRepositoryUrl } from './workspace-repository'

describe('canonicalRepositoryCloneUrl', () => {
  it('converts GitHub SSH roots to canonical HTTPS', () => {
    expect(canonicalRepositoryCloneUrl('git@github.com:owner/repo.git')).toBe('https://github.com/owner/repo')
    expect(canonicalRepositoryCloneUrl('git@github.com:owner/repo')).toBe('https://github.com/owner/repo')
  })

  it('normalizes HTTPS GitHub forms', () => {
    expect(canonicalRepositoryCloneUrl('https://github.com/owner/repo.git')).toBe('https://github.com/owner/repo')
    expect(canonicalRepositoryCloneUrl('  https://github.com/owner/repo  ')).toBe('https://github.com/owner/repo')
  })

  it('leaves non-GitHub URLs unchanged', () => {
    expect(canonicalRepositoryCloneUrl('https://gitlab.example.com/owner/repo.git')).toBe(
      'https://gitlab.example.com/owner/repo.git',
    )
    expect(canonicalRepositoryCloneUrl('git@gitlab.example.com:owner/repo.git')).toBe(
      'git@gitlab.example.com:owner/repo.git',
    )
  })
})

describe('validateWorkspaceRepositoryUrl', () => {
  it('accepts empty input', () => {
    expect(validateWorkspaceRepositoryUrl('Source repository URL', '   ')).toEqual({ url: '' })
  })

  it('accepts canonicalized GitHub SSH roots', () => {
    expect(validateWorkspaceRepositoryUrl('Source repository URL', 'git@github.com:owner/repo.git')).toEqual({
      url: 'https://github.com/owner/repo',
    })
  })

  it('accepts canonically escaped paths', () => {
    expect(validateWorkspaceRepositoryUrl('Source repository URL', 'https://gitlab.example.com/owner/re%20po')).toEqual(
      { url: 'https://gitlab.example.com/owner/re%20po' },
    )
  })

  it('accepts credential-free non-GitHub HTTPS URLs', () => {
    expect(validateWorkspaceRepositoryUrl('Source repository URL', 'https://gitlab.example.com/owner/repo.git')).toEqual(
      { url: 'https://gitlab.example.com/owner/repo.git' },
    )
  })

  it.each([
    ['plain HTTP', 'http://github.com/owner/repo'],
    ['embedded credentials', 'https://user:token@github.com/owner/repo'],
    ['query string', 'https://gitlab.example.com/owner/repo?x=1'],
    ['fragment', 'https://gitlab.example.com/owner/repo#main'],
    ['non-GitHub SSH root', 'git@gitlab.example.com:owner/repo.git'],
    ['non-default port', 'https://gitlab.example.com:8443/owner/repo'],
    ['empty path', 'https://gitlab.example.com/'],
    ['trailing slash', 'https://gitlab.example.com/owner/repo/'],
    ['empty path segment', 'https://gitlab.example.com/owner//repo'],
    ['escaped path separator', 'https://gitlab.example.com/owner%2Frepo'],
    ['non-canonical escaped character', 'https://gitlab.example.com/owner/repo%41'],
  ])('rejects %s', (_name, url) => {
    const result = validateWorkspaceRepositoryUrl('Source repository URL', url)
    expect(result).toHaveProperty('error')
    expect((result as { error: string }).error).toContain('Source repository URL')
  })
})
