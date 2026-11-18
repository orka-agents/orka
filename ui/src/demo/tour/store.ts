import { create } from 'zustand'
import { TOUR_STEPS } from './steps'

const seenKey = 'orka-demo-tour-seen'

function hasSeenTour(): boolean {
  try {
    return localStorage.getItem(seenKey) !== null
  } catch {
    // Without storage every visit is a first visit.
    return false
  }
}

function rememberTourSeen() {
  try {
    localStorage.setItem(seenKey, '1')
  } catch {
    // The tour opens again on the next visit.
  }
}

interface TourState {
  open: boolean
  /** Index into TOUR_STEPS. */
  step: number
  /** The narrow-screen panel is folded down to one line. */
  collapsed: boolean
  start: () => void
  goTo: (step: number) => void
  close: () => void
  setCollapsed: (collapsed: boolean) => void
}

/** The tour opens by itself on a first visit and from the sidebar after that. */
export const useTourStore = create<TourState>()((set) => ({
  open: !hasSeenTour(),
  step: 0,
  collapsed: false,
  start: () => set({ open: true, step: 0, collapsed: false }),
  goTo: (step) => set({ step: Math.max(0, Math.min(step, TOUR_STEPS.length - 1)) }),
  close: () => {
    rememberTourSeen()
    set({ open: false })
  },
  setCollapsed: (collapsed) => set({ collapsed }),
}))
