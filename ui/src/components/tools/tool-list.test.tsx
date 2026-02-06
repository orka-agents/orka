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
    useLocation: () => ({ pathname: '/tools' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { ToolList } from './tool-list'

describe('ToolList', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('loading state shows skeleton rows', () => {
    const { container } = render(<ToolList />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('empty state shows "No tools found."', async () => {
    render(<ToolList />)
    await waitFor(() => {
      expect(screen.getByText('No tools found.')).toBeInTheDocument()
    })
  })

  it('populated table shows tools with correct data', async () => {
    server.use(
      http.get('/api/v1/tools', () =>
        HttpResponse.json({
          items: [
            { name: 'create_task', builtin: true, description: 'Create a task', available: true },
            { name: 'custom-tool', namespace: 'default', builtin: false, description: 'A custom tool', available: false, url: 'http://example.com' },
          ],
          metadata: {},
        })
      )
    )

    render(<ToolList />)
    await waitFor(() => {
      expect(screen.getByText('create_task')).toBeInTheDocument()
    })
    expect(screen.getByText('custom-tool')).toBeInTheDocument()
    expect(screen.getByText('Create a task')).toBeInTheDocument()
    expect(screen.getByText('A custom tool')).toBeInTheDocument()
  })

  it('built-in tools show "Built-in" badge', async () => {
    server.use(
      http.get('/api/v1/tools', () =>
        HttpResponse.json({
          items: [
            { name: 'create_task', builtin: true, description: 'Create a task', available: true },
          ],
          metadata: {},
        })
      )
    )

    render(<ToolList />)
    await waitFor(() => {
      expect(screen.getByText('Built-in')).toBeInTheDocument()
    })
  })

  it('custom tools show "Custom" badge', async () => {
    server.use(
      http.get('/api/v1/tools', () =>
        HttpResponse.json({
          items: [
            { name: 'custom-tool', namespace: 'default', builtin: false, description: 'A custom tool', available: false },
          ],
          metadata: {},
        })
      )
    )

    render(<ToolList />)
    await waitFor(() => {
      expect(screen.getByText('Custom')).toBeInTheDocument()
    })
  })

  it('available tool shows "Available" status badge', async () => {
    server.use(
      http.get('/api/v1/tools', () =>
        HttpResponse.json({
          items: [
            { name: 'my-tool', builtin: false, description: 'desc', available: true },
          ],
          metadata: {},
        })
      )
    )

    render(<ToolList />)
    await waitFor(() => {
      expect(screen.getByText('Available')).toBeInTheDocument()
    })
  })

  it('unavailable tool shows "Unavailable" status badge', async () => {
    server.use(
      http.get('/api/v1/tools', () =>
        HttpResponse.json({
          items: [
            { name: 'my-tool', builtin: false, description: 'desc', available: false },
          ],
          metadata: {},
        })
      )
    )

    render(<ToolList />)
    await waitFor(() => {
      expect(screen.getByText('Unavailable')).toBeInTheDocument()
    })
  })
})
