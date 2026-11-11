import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { PRStatusBadge } from './pr-status-badge'

const receipt = { id: 'PR_kwDOabc', number: 42, url: 'https://github.com/org/repo/pull/42', state: 'Open' }

describe('PRStatusBadge', () => {
  it('renders nothing without a pull request receipt', () => {
    const { container } = render(<PRStatusBadge />)
    expect(container.innerHTML).toBe('')
  })

  it('shows the number and the controller-reported state', () => {
    render(<PRStatusBadge receipt={receipt} />)
    expect(screen.getByText('PR #42 open')).toBeInTheDocument()
  })

  it('links to the pull request in a new tab', () => {
    render(<PRStatusBadge receipt={{ ...receipt, state: 'Merged' }} />)
    const link = screen.getByText('PR #42 merged').closest('a')
    expect(link).toHaveAttribute('href', 'https://github.com/org/repo/pull/42')
    expect(link).toHaveAttribute('target', '_blank')
  })

  it('does not link a non-http URL', () => {
    render(<PRStatusBadge receipt={{ ...receipt, url: 'javascript:alert(1)' }} />)
    expect(screen.getByText('PR #42 open').closest('a')).toBeNull()
  })

  it('still shows a receipt without a number or state', () => {
    render(<PRStatusBadge receipt={{ id: 'PR_kwDOabc' }} />)
    expect(screen.getByText('PR')).toBeInTheDocument()
  })
})
