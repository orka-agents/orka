import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/providers' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { ProviderList } from './provider-list'

describe('ProviderList', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('shows an empty state', async () => {
    render(<ProviderList />)
    await waitFor(() => {
      expect(screen.getByText(/no providers yet/i)).toBeInTheDocument()
    })
  })

  it('lists providers with type, model, and readiness', async () => {
    server.use(
      http.get('/api/v1/providers', () =>
        HttpResponse.json({
          items: [
            { name: 'anthropic', namespace: 'default', type: 'anthropic', defaultModel: 'claude-sonnet-4', ready: true },
            { name: 'proxy', namespace: 'default', type: 'openai', defaultModel: '', ready: false },
          ],
          metadata: {},
        }),
      ),
    )
    render(<ProviderList />)
    await waitFor(() => expect(screen.getByRole('link', { name: 'anthropic' })).toBeInTheDocument())
    expect(screen.getByText('claude-sonnet-4')).toBeInTheDocument()
    expect(screen.getByText('Ready')).toBeInTheDocument()
    expect(screen.getByText('Not ready')).toBeInTheDocument()
  })

  it('offers a create action', async () => {
    render(<ProviderList />)
    expect(screen.getByRole('link', { name: /new provider/i })).toHaveAttribute('href', '/providers/new')
  })
})
