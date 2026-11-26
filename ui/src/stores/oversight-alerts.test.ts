import { beforeEach, describe, expect, it } from 'vitest'
import { unreadAlertCount, useOversightAlerts } from './oversight-alerts'

const alert = (decisionID: string, namespace: string) => ({
  decisionID, namespace, taskName: 't', observedAt: '2026-08-31T00:00:00Z', rule: 'Rule 1', sentence: 'broke rule 1',
})

describe('useOversightAlerts', () => {
  beforeEach(() => useOversightAlerts.getState().reset())

  it('marks only the given namespace read', () => {
    const store = useOversightAlerts.getState()
    store.add(alert('a@1', 'team'))
    store.add(alert('b@1', 'other'))
    useOversightAlerts.getState().markAllRead('team')
    const state = useOversightAlerts.getState()
    expect(unreadAlertCount(state, 'team')).toBe(0)
    expect(unreadAlertCount(state, 'other')).toBe(1)
  })
})
