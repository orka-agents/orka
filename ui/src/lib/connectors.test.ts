import { describe, it, expect, beforeEach } from 'vitest'
import { callbackReasonMessage, clearCompletionFragment, completionCommand, readCompletionToken } from './connectors'

describe('connectors helpers', () => {
  beforeEach(() => window.history.replaceState(null, '', '/settings/connectors'))

  it('reads the completion token and clears it only when asked', () => {
    window.history.replaceState(null, '', '/settings/connectors?status=pending&connection=c#completion=tok%2B1')
    expect(readCompletionToken()).toBe('tok+1')
    expect(readCompletionToken()).toBe('tok+1')
    clearCompletionFragment()
    expect(window.location.hash).toBe('')
    expect(window.location.search).toBe('?status=pending&connection=c')
    expect(readCompletionToken()).toBeNull()
  })

  it('ignores fragments without a completion', () => {
    window.history.replaceState(null, '', '/settings/connectors#other=1')
    expect(readCompletionToken()).toBeNull()
  })

  it('names the CLI fallback with the sealed namespace', () => {
    expect(completionCommand('github-abc', 'team-a')).toBe('orka connection complete github-abc --namespace team-a --completion <value from the address bar>')
    expect(completionCommand('github-abc', undefined)).toBe('orka connection complete github-abc --completion <value from the address bar>')
  })

  it('words the callback reasons', () => {
    expect(callbackReasonMessage('scopes_denied')).toMatch(/fewer permissions/)
    expect(callbackReasonMessage('provider_changed')).toMatch(/changed/)
    expect(callbackReasonMessage(undefined)).toBe('Linking failed.')
    expect(callbackReasonMessage('weird')).toBe('Linking failed (weird).')
  })
})
