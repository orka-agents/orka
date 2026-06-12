import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { StatusDot } from './status-dot'

describe('StatusDot', () => {
  it.each([
    ['Pending', 'bg-status-pending'],
    ['Running', 'bg-status-running'],
    ['Succeeded', 'bg-status-succeeded'],
    ['Failed', 'bg-status-failed'],
  ] as const)('renders a %s dot in the phase color', (phase, dotClass) => {
    render(<StatusDot phase={phase} />)
    const dot = screen.getByTestId('status-dot')
    expect(dot.className).toContain(dotClass)
  })

  it('renders the visible label text', () => {
    render(<StatusDot phase="Succeeded" />)
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
  })

  it('falls back to Pending styling for an unknown phase', () => {
    render(<StatusDot phase="Bogus" />)
    expect(screen.getByTestId('status-dot').className).toContain('bg-status-pending')
  })

  it('preserves the literal phase text for unknown phases (no silent relabel)', () => {
    // Regression guard: an unrecognized/custom backend phase must keep its own
    // text — only the dot *color* falls back to Pending, never the label.
    render(<StatusDot phase="Terminating" />)
    expect(screen.getByText('Terminating')).toBeInTheDocument()
    expect(screen.queryByText('Pending')).not.toBeInTheDocument()
    expect(screen.getByTestId('status-dot').className).toContain('bg-status-pending')
  })

  it('uses the style label only when no phase is supplied', () => {
    render(<StatusDot />)
    expect(screen.getByText('Pending')).toBeInTheDocument()
  })

  it('applies the live pulse for Running by default', () => {
    render(<StatusDot phase="Running" />)
    expect(screen.getByTestId('status-dot').className).toContain('animate-pulse-live')
  })

  it('does NOT pulse terminal/pending phases by default', () => {
    for (const phase of ['Pending', 'Succeeded', 'Failed'] as const) {
      const { unmount } = render(<StatusDot phase={phase} />)
      expect(screen.getByTestId('status-dot').className).not.toContain('animate-pulse-live')
      unmount()
    }
  })

  it('honors an explicit pulse override', () => {
    render(<StatusDot phase="Succeeded" pulse />)
    expect(screen.getByTestId('status-dot').className).toContain('animate-pulse-live')
  })

  it('can suppress the pulse even when Running', () => {
    render(<StatusDot phase="Running" pulse={false} />)
    expect(screen.getByTestId('status-dot').className).not.toContain('animate-pulse-live')
  })

  it('can hide the visible label but still expose it to screen readers', () => {
    render(<StatusDot phase="Running" hideLabel />)
    // Label text remains in the accessibility tree (sr-only), so color is
    // never the only signal — it's just visually hidden.
    expect(screen.getByText('Running')).toHaveClass('sr-only')
    expect(screen.getByTestId('status-dot')).toBeInTheDocument()
  })
})
