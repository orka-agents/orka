import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { Inbox } from 'lucide-react'
import { EmptyState } from './empty-state'

describe('EmptyState', () => {
  it('renders the headline', () => {
    render(<EmptyState headline="No tasks yet" />)
    expect(screen.getByText('No tasks yet')).toBeInTheDocument()
  })

  it('renders the hint when provided', () => {
    render(<EmptyState headline="No tasks yet" hint="Create one to get started" />)
    expect(screen.getByText('Create one to get started')).toBeInTheDocument()
  })

  it('renders an icon when provided', () => {
    const { container } = render(<EmptyState headline="Empty" icon={Inbox} />)
    expect(container.querySelector('svg')).toBeInTheDocument()
  })

  it('renders a CTA and fires its handler', async () => {
    const onClick = vi.fn()
    render(
      <EmptyState
        headline="No tasks yet"
        action={<button onClick={onClick}>Create task</button>}
      />,
    )
    await userEvent.click(screen.getByRole('button', { name: 'Create task' }))
    expect(onClick).toHaveBeenCalledOnce()
  })
})
