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
  }
})

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { TaskChildrenTable } from './task-children-table'

describe('TaskChildrenTable', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('shows a message when the API resolves no children', async () => {
    render(<TaskChildrenTable taskId="parent" />)
    await waitFor(() => expect(screen.getByText(/no child tasks/i)).toBeInTheDocument())
  })

  it('lists resolved children with type, agent, and phase', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/children', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'child-1', namespace: 'default', uid: 'c1' },
              spec: { type: 'agent', agentRef: { name: 'coder' } },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 'child-2', namespace: 'default', uid: 'c2' },
              spec: { type: 'ai' },
              status: { phase: 'Succeeded' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskChildrenTable taskId="parent" />)
    await waitFor(() => expect(screen.getByText('child-1')).toBeInTheDocument())
    expect(screen.getByText('coder')).toBeInTheDocument()
    expect(screen.getByText('child-2')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
  })
})
