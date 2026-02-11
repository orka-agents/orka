import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => {
      let href = to
      if (params) {
        for (const [key, value] of Object.entries(params)) {
          href = href.replace(`$${key}`, value as string)
        }
      }
      return <a href={href} {...props}>{children}</a>
    },
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/tasks' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { ChildTaskTree } from './child-task-tree'

describe('ChildTaskTree', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows loading skeletons', () => {
    server.use(
      http.get('/api/v1/tasks/:id/children', async () => {
        await new Promise((r) => setTimeout(r, 5000))
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )
    const { container } = render(<ChildTaskTree parentTaskName="parent" />)
    expect(container.querySelector('[data-testid="child-task-loading"]')).toBeInTheDocument()
  })

  it('shows empty state when no children', async () => {
    render(<ChildTaskTree parentTaskName="parent" />)
    await waitFor(() => {
      expect(screen.getByText('No child tasks')).toBeInTheDocument()
    })
  })

  it('renders children with phase badges', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/children', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'child-a', namespace: 'default' },
              spec: { type: 'ai', agentRef: { name: 'coder' } },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 'child-b', namespace: 'default' },
              spec: { type: 'ai' },
              status: { phase: 'Succeeded', message: 'All done' },
            },
          ],
          metadata: {},
        }),
      ),
    )

    render(<ChildTaskTree parentTaskName="parent" />)
    await waitFor(() => {
      expect(screen.getByText('child-a')).toBeInTheDocument()
    })
    expect(screen.getByText('child-b')).toBeInTheDocument()
    expect(screen.getByText('coder')).toBeInTheDocument()
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('All done')).toBeInTheDocument()
  })

  it('renders navigation links', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/children', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'child-link', namespace: 'default' },
              spec: { type: 'ai' },
              status: { phase: 'Pending' },
            },
          ],
          metadata: {},
        }),
      ),
    )

    render(<ChildTaskTree parentTaskName="parent" />)
    await waitFor(() => {
      expect(screen.getByText('child-link')).toBeInTheDocument()
    })
    const link = screen.getByText('child-link').closest('a')
    expect(link).toHaveAttribute('href', '/tasks/child-link')
  })
})
