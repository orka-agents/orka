import { describe, it, expect } from 'vitest'
import { http, HttpResponse } from 'msw'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

import { server } from '@/test/mocks/server'
import { IdentityPopover } from './identity-popover'

describe('IdentityPopover', () => {
  it('shows the verified identity from whoami', async () => {
    const user = userEvent.setup()
    render(<IdentityPopover />)
    await user.click(screen.getByRole('button', { name: /account identity/i }))
    expect(await screen.findByText('Signed in')).toBeInTheDocument()
    expect(screen.getByText('kubernetes')).toBeInTheDocument()
    expect(screen.getByText(/serviceaccount/)).toBeInTheDocument()
  })

  it('renders transaction token details when present', async () => {
    server.use(
      http.get('/api/v1/auth/whoami', () =>
        HttpResponse.json({
          authenticated: true,
          authType: 'context-token',
          subject: 'workload-a',
          transaction: { profile: 'transaction-token', id: 'tx-42', scopes: ['orka:tasks:create'] },
        }),
      ),
    )
    const user = userEvent.setup()
    render(<IdentityPopover />)
    await user.click(screen.getByRole('button', { name: /account identity/i }))
    expect(await screen.findByText('Transaction token')).toBeInTheDocument()
    expect(screen.getByText('tx-42')).toBeInTheDocument()
    expect(screen.getByText('orka:tasks:create')).toBeInTheDocument()
  })

  it('shows a helpful message when whoami fails', async () => {
    server.use(
      http.get('/api/v1/auth/whoami', () => new HttpResponse(null, { status: 403 })),
    )
    const user = userEvent.setup()
    render(<IdentityPopover />)
    await user.click(screen.getByRole('button', { name: /account identity/i }))
    expect(await screen.findByText(/identity lookup failed/i)).toBeInTheDocument()
  })
})
