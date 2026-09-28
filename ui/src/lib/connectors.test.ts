import { describe, it, expect, beforeEach } from 'vitest'
import { callbackReasonMessage, takeCompletionToken } from './connectors'

describe('connectors helpers', () => {
  beforeEach(() => window.history.replaceState(null, '', '/settings/connectors'))

  it('takes the completion token out of the fragment once', () => {
    window.history.replaceState(null, '', '/settings/connectors?status=pending&connection=c#completion=tok%2B1')
    expect(takeCompletionToken()).toBe('tok+1')
    expect(window.location.hash).toBe('')
    expect(window.location.search).toBe('?status=pending&connection=c')
    expect(takeCompletionToken()).toBeNull()
  })

  it('ignores fragments without a completion', () => {
    window.history.replaceState(null, '', '/settings/connectors#other=1')
    expect(takeCompletionToken()).toBeNull()
  })

  it('words the callback reasons', () => {
    expect(callbackReasonMessage('scopes_denied')).toMatch(/fewer permissions/)
    expect(callbackReasonMessage('provider_changed')).toMatch(/changed/)
    expect(callbackReasonMessage(undefined)).toBe('Linking failed.')
    expect(callbackReasonMessage('weird')).toBe('Linking failed (weird).')
  })
})
