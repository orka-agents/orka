import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@/test/test-utils'

const mockNavigate = vi.fn()
const mockSend = vi.fn()

vi.mock('@tanstack/react-router', async () => ({
  ...(await vi.importActual('@tanstack/react-router')),
  useNavigate: () => mockNavigate,
  Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
}))

vi.mock('@/hooks/use-chat', () => ({ useSendMessage: () => mockSend }))

import { TOUR_STEPS, TOUR_STOPS, INCIDENT_TASK } from './steps'
import { useTourStore } from './store'
import { TourLauncher, TourPanel } from './tour'

const seenKey = 'orka-demo-tour-seen'
const next = () => fireEvent.click(screen.getByRole('button', { name: 'Next' }))

// The test environment has no usable localStorage.
function memoryStorage() {
  const items = new Map<string, string>()
  return {
    getItem: (key: string) => items.get(key) ?? null,
    setItem: (key: string, value: string) => void items.set(key, value),
    removeItem: (key: string) => void items.delete(key),
    clear: () => items.clear(),
  }
}

describe('demo tour', () => {
  beforeEach(() => {
    mockNavigate.mockClear()
    mockSend.mockClear()
    vi.stubGlobal('localStorage', memoryStorage())
    useTourStore.setState({ open: true, step: 0, collapsed: false })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('opens by itself only until the visitor has seen it', async () => {
    vi.resetModules()
    expect((await import('./store')).useTourStore.getState().open).toBe(true)
    localStorage.setItem(seenKey, '1')
    vi.resetModules()
    expect((await import('./store')).useTourStore.getState().open).toBe(false)
  })

  it('welcomes the visitor and lets them skip the tour for good', () => {
    render(<TourPanel slot="bottom" />)
    expect(screen.getByRole('heading', { name: 'Welcome to the Orka demo' })).toBeInTheDocument()
    expect(mockNavigate).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: 'Explore on my own' }))
    expect(screen.queryByRole('complementary', { name: 'Demo tour' })).not.toBeInTheDocument()
    expect(localStorage.getItem(seenKey)).not.toBeNull()
  })

  it('walks the stops in order, showing each page', () => {
    render(<TourPanel slot="bottom" />)
    fireEvent.click(screen.getByRole('button', { name: 'Start the tour' }))
    expect(mockNavigate).toHaveBeenLastCalledWith({ to: '/tasks/$taskId', params: { taskId: INCIDENT_TASK }, search: {} })
    expect(screen.getByText(`Step 1 of ${TOUR_STOPS.length}`)).toBeInTheDocument()

    next()
    expect(mockNavigate).toHaveBeenLastCalledWith({ to: '/tasks/$taskId', params: { taskId: INCIDENT_TASK }, search: { tab: 'trace' } })
    next()
    expect(mockNavigate).toHaveBeenLastCalledWith(expect.objectContaining({ search: { tab: 'approvals' } }))

    fireEvent.click(screen.getByRole('button', { name: 'Back' }))
    expect(screen.getByRole('heading', { name: 'See what it did' })).toBeInTheDocument()
  })

  it('sends a suggested prompt from the chat stop', () => {
    useTourStore.setState({ step: TOUR_STEPS.findIndex((step) => step.prompts) })
    render(<TourPanel slot="bottom" />)
    fireEvent.click(screen.getByRole('button', { name: 'Review PR #1486 in checkout-service' }))
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/chat' })
    expect(mockSend).toHaveBeenCalledWith('Review PR #1486 in checkout-service')
  })

  it('finishes on the closing step and remembers it', () => {
    useTourStore.setState({ step: TOUR_STEPS.length - 1 })
    render(<TourPanel slot="bottom" />)
    expect(screen.getByRole('link', { name: /Security/ })).toHaveAttribute('href', '/security')
    fireEvent.click(screen.getByRole('button', { name: 'Finish' }))
    expect(useTourStore.getState().open).toBe(false)
    expect(localStorage.getItem(seenKey)).not.toBeNull()
  })

  it('folds the narrow-screen panel to its title', () => {
    render(<TourPanel slot="bottom" />)
    fireEvent.click(screen.getByRole('button', { name: 'Collapse the tour' }))
    expect(screen.queryByRole('button', { name: 'Start the tour' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Welcome to the Orka demo' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Expand the tour' }))
    expect(screen.getByRole('button', { name: 'Start the tour' })).toBeInTheDocument()
  })

  it('renders only in the slot for the current width', () => {
    // jsdom has no matchMedia, so the viewport counts as narrow.
    render(<TourPanel slot="rail" />)
    expect(screen.queryByRole('complementary', { name: 'Demo tour' })).not.toBeInTheDocument()
  })

  it('restarts from the sidebar', () => {
    useTourStore.setState({ open: false, step: 4 })
    const onSelect = vi.fn()
    render(<TourLauncher collapsed onSelect={onSelect} />)
    fireEvent.click(screen.getByRole('button', { name: 'Take the tour' }))
    expect(useTourStore.getState()).toMatchObject({ open: true, step: 0 })
    expect(onSelect).toHaveBeenCalled()
  })
})
