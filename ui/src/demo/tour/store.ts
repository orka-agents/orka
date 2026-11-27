import { create } from 'zustand'
import { TOUR_STEPS } from './steps'

const seenKey = 'orka-demo-tour-seen'

function hasSeenTour(): boolean {
  try {
    return localStorage.getItem(seenKey) !== null
  } catch {
    return false
  }
}

function rememberTourSeen() {
  try {
    localStorage.setItem(seenKey, '1')
  } catch {
  }
}

interface TourState {
  open: boolean
  step: number
  collapsed: boolean
  start: () => void
  goTo: (step: number) => void
  close: () => void
  setCollapsed: (collapsed: boolean) => void
}

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
