import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const navigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => navigate,
    useLocation: () => ({ pathname: '/providers/anthropic' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { ProviderDetail } from './provider-detail'

describe('ProviderDetail', () => {
  beforeEach(() => {
    navigate.mockReset()
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('renders configuration and status', async () => {
    server.use(
      http.get('/api/v1/providers/:name', ({ params }) =>
        HttpResponse.json({
          metadata: { name: params.name, namespace: 'default' },
          spec: {
            type: 'anthropic',
            secretRef: { name: 'anthropic-key', key: 'api-key' },
            defaultModel: 'claude-sonnet-4',
            rateLimit: { requestsPerMinute: 60 },
          },
          status: { ready: true, message: 'validated' },
        }),
      ),
    )
    render(<ProviderDetail providerName="anthropic" />)
    await waitFor(() => expect(screen.getByRole('heading', { name: 'anthropic' })).toBeInTheDocument())
    expect(screen.getByText('anthropic-key')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet-4')).toBeInTheDocument()
    expect(screen.getByText('Ready')).toBeInTheDocument()
    expect(screen.getByText('60')).toBeInTheDocument()
  })

  it('deletes after a two-step confirm', async () => {
    let deleted = false
    server.use(
      http.delete('/api/v1/providers/:name', () => {
        deleted = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<ProviderDetail providerName="anthropic" />)
    await waitFor(() => expect(screen.getByRole('heading', { name: 'anthropic' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /^delete$/i }))
    expect(deleted).toBe(false)
    await user.click(screen.getByRole('button', { name: /confirm delete/i }))
    await waitFor(() => expect(deleted).toBe(true))
    expect(navigate).toHaveBeenCalledWith({ to: '/providers' })
  })

  it('shows a not-found message on error', async () => {
    server.use(
      http.get('/api/v1/providers/:name', () => new HttpResponse(null, { status: 404 })),
    )
    render(<ProviderDetail providerName="ghost" />)
    await waitFor(() => expect(screen.getByText(/not found/i)).toBeInTheDocument())
  })
})
