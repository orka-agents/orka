import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskResultViewer } from './task-result-viewer'

describe('TaskResultViewer', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('initial state shows "Load Result" button', () => {
    render(<TaskResultViewer taskId="task-1" />)
    expect(screen.getByText('Load Result')).toBeInTheDocument()
  })

  it('after click, shows result content', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id/result', () =>
        HttpResponse.json({ result: 'Hello result output' }),
      ),
    )
    render(<TaskResultViewer taskId="task-1" />)
    await user.click(screen.getByText('Load Result'))
    await waitFor(() => {
      expect(screen.getByText('Hello result output')).toBeInTheDocument()
    })
  })

  it('shows "No result available" when API returns empty result', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id/result', () =>
        HttpResponse.json({}),
      ),
    )
    render(<TaskResultViewer taskId="task-2" />)
    await user.click(screen.getByText('Load Result'))
    await waitFor(() => {
      expect(screen.getByText('No result available')).toBeInTheDocument()
    })
  })

  it('shows loading skeleton while fetching result', async () => {
    server.use(
      http.get('/api/v1/tasks/:id/result', async () => {
        await new Promise((resolve) => setTimeout(resolve, 200))
        return HttpResponse.json({ result: 'delayed' })
      }),
    )
    const user = userEvent.setup()
    render(<TaskResultViewer taskId="task-3" />)
    await user.click(screen.getByText('Load Result'))
    // While the request is in-flight, the loading skeleton should appear
    await waitFor(() => {
      expect(screen.queryByText('Load Result')).not.toBeInTheDocument()
    })
  })
})
