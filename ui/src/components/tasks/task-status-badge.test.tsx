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

  it('uses yellow classes for Pending', () => {
    render(<TaskStatusBadge phase="Pending" />)
    expect(screen.getByText('Pending').className).toContain('bg-yellow-100')
  })

  it('uses blue classes for Running', () => {
    render(<TaskStatusBadge phase="Running" />)
    expect(screen.getByText('Running').className).toContain('bg-blue-100')
  })

  it('uses green classes for Succeeded', () => {
    render(<TaskStatusBadge phase="Succeeded" />)
    expect(screen.getByText('Succeeded').className).toContain('bg-green-100')
  })

  it('uses red classes for Failed', () => {
    render(<TaskStatusBadge phase="Failed" />)
    expect(screen.getByText('Failed').className).toContain('bg-red-100')
  })

  it('falls back to Pending style for unknown phase', () => {
    render(<TaskStatusBadge phase="Unknown" />)
    expect(screen.getByText('Unknown').className).toContain('bg-yellow-100')
  })
})
