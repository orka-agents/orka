import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useLocation: () => ({ pathname: '/no-such-page' }),
  }
})

import { render, screen } from '@/test/test-utils'
import { ApiError } from '@/lib/api-client'
import { AccessErrorView, NotFoundPage } from './not-found-page'

describe('NotFoundPage', () => {
  beforeEach(() => {
    document.title = 'Orka'
  })

  it('names the missing address and links back to the Dashboard', () => {
    render(<NotFoundPage />)
    expect(screen.getByRole('heading', { level: 1, name: 'Page not found' })).toBeInTheDocument()
    expect(screen.getByText('Nothing lives at /no-such-page')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Go to Dashboard' })).toHaveAttribute('href', '/')
    expect(document.title).toBe('Page not found · Orka')
  })

  it('leaves the top padding to the card', () => {
    const { container } = render(<NotFoundPage />)
    // Card already pads its top (py-6); a second pt-6 on the content doubled it.
    expect(container.querySelector('[data-slot="card-content"]')).not.toHaveClass('pt-6')
  })
})

describe('AccessErrorView', () => {
  beforeEach(() => {
    document.title = 'Orka'
  })

  it('titles a forbidden detail page and keeps a way back', () => {
    render(
      <AccessErrorView
        title="Could not load repository"
        resource="repository payments"
        error={new ApiError(403, '{"error":{"code":403,"message":"not authorized"}}')}
        action={<a href="/security">Back to Security</a>}
      />,
    )
    expect(screen.getByRole('heading', { level: 1, name: 'Could not load repository' })).toBeInTheDocument()
    expect(document.title).toBe('Could not load repository · Orka')
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Not authorized to view repository payments')
    expect(alert).toHaveTextContent('Your token lacks read permission for repository payments (not authorized).')
    expect(screen.getByRole('link', { name: 'Back to Security' })).toHaveAttribute('href', '/security')
  })

  it('reports other failures with their message', () => {
    render(
      <AccessErrorView
        title="Could not load repository"
        resource="repository payments"
        error={new ApiError(500, '{"error":{"code":500,"message":"store unavailable"}}')}
        action={<a href="/security">Back to Security</a>}
      />,
    )
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Could not load repository payments')
    expect(alert).toHaveTextContent('store unavailable')
  })
})
