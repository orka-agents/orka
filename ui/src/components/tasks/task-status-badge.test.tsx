import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { TaskStatusBadge } from './task-status-badge'

describe('TaskStatusBadge', () => {
  it('renders "Pending" when no phase provided', () => {
    render(<TaskStatusBadge />)
    expect(screen.getByText('Pending')).toBeInTheDocument()
  })

  it.each(['Pending', 'Running', 'Succeeded', 'Failed'] as const)(
    'renders correct text for phase %s',
    (phase) => {
      render(<TaskStatusBadge phase={phase} />)
      expect(screen.getByText(phase)).toBeInTheDocument()
    },
  )

  it.each([
    ['Pending', 'bg-status-pending'],
    ['Running', 'bg-status-running'],
    ['Succeeded', 'bg-status-succeeded'],
    ['Failed', 'bg-status-failed'],
  ] as const)('uses the %s status token (not a pastel)', (phase, dotClass) => {
    render(<TaskStatusBadge phase={phase} />)
    const dot = screen.getByTestId('status-dot')
    expect(dot.className).toContain(dotClass)
    // No legacy template pastel survives.
    expect(dot.className).not.toMatch(/bg-(yellow|blue|green|red)-(100|800|900)/)
  })

  it('renders a colored status dot alongside the label', () => {
    render(<TaskStatusBadge phase="Running" />)
    expect(screen.getByTestId('status-dot')).toBeInTheDocument()
    expect(screen.getByText('Running')).toBeInTheDocument()
  })

  it('falls back to Pending style for unknown phase', () => {
    render(<TaskStatusBadge phase="Unknown" />)
    expect(screen.getByText('Unknown')).toBeInTheDocument()
    // Unknown phases inherit the Pending dot color.
    expect(screen.getByTestId('status-dot').className).toContain('bg-status-pending')
  })
})
