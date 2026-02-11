import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { PRStatusBadge } from './pr-status-badge'

describe('PRStatusBadge', () => {
  it('returns null when no annotations', () => {
    const { container } = render(<PRStatusBadge />)
    expect(container.innerHTML).toBe('')
  })

  it('returns null when no PR annotations', () => {
    const { container } = render(<PRStatusBadge annotations={{ foo: 'bar' }} />)
    expect(container.innerHTML).toBe('')
  })

  it('shows PR badge with number and status', () => {
    render(<PRStatusBadge annotations={{
      'mercan.ai/pr-url': 'https://github.com/org/repo/pull/42',
      'mercan.ai/pr-number': '42',
      'mercan.ai/pr-status': 'open',
    }} />)
    expect(screen.getByText(/PR #42/)).toBeInTheDocument()
    expect(screen.getByText(/open/)).toBeInTheDocument()
  })

  it('links to PR URL', () => {
    render(<PRStatusBadge annotations={{
      'mercan.ai/pr-url': 'https://github.com/org/repo/pull/42',
      'mercan.ai/pr-number': '42',
      'mercan.ai/pr-status': 'merged',
    }} />)
    const link = screen.getByText(/PR #42/).closest('a')
    expect(link).toHaveAttribute('href', 'https://github.com/org/repo/pull/42')
    expect(link).toHaveAttribute('target', '_blank')
  })
})
