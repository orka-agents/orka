import { create } from 'zustand'

/** One charter alert raised in this browser session. Nothing is sent anywhere. */
export interface CharterAlert {
  decisionID: string
  namespace: string
  taskName: string
  agentName?: string
  observedAt: string
  /** "Rule 2" or "rule B1", with the charter it belongs to when declared. */
  rule: string
  charter?: string
  sentence: string
  read: boolean
}

interface OversightAlertsState {
  alerts: CharterAlert[]
  /** Adds an alert unless one exists for the decision; returns whether it was new. */
  add: (alert: Omit<CharterAlert, 'read'>) => boolean
  markAllRead: () => void
  reset: () => void
}

/** Most alerts kept per session; the oldest drop off. */
const MAX_ALERTS = 200

// Session-only: alerts are not persisted, and "read" is a client-side mark.
export const useOversightAlerts = create<OversightAlertsState>()((set, get) => ({
  alerts: [],
  add: (alert) => {
    if (get().alerts.some((a) => a.decisionID === alert.decisionID && a.namespace === alert.namespace)) return false
    set((state) => ({ alerts: [{ ...alert, read: false }, ...state.alerts].slice(0, MAX_ALERTS) }))
    return true
  },
  markAllRead: () => set((state) => ({ alerts: state.alerts.map((a) => (a.read ? a : { ...a, read: true })) })),
  reset: () => set({ alerts: [] }),
}))

export const unreadAlertCount = (state: OversightAlertsState, namespace: string) =>
  state.alerts.filter((a) => !a.read && a.namespace === namespace).length
