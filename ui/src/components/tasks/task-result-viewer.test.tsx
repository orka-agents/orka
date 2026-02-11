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

  it('renders structured result with verdict badge', async () => {
    const structured = JSON.stringify({
      summary: 'All tests pass',
      verdict: 'APPROVE',
      feedback: 'Looks good',
      files: ['src/main.go'],
      pushBranch: 'feature/test',
    })
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id/result', () =>
        HttpResponse.json({ result: structured }),
      ),
    )
    render(<TaskResultViewer taskId="task-structured" />)
    await user.click(screen.getByText('Load Result'))
    await waitFor(() => {
      expect(screen.getByTestId('verdict-badge')).toHaveTextContent('APPROVE')
    })
    expect(screen.getByText('All tests pass')).toBeInTheDocument()
    expect(screen.getByText('Looks good')).toBeInTheDocument()
    expect(screen.getByText('src/main.go')).toBeInTheDocument()
    expect(screen.getByText('feature/test')).toBeInTheDocument()
  })

  it('renders REQUEST_CHANGES verdict with red styling', async () => {
    const structured = JSON.stringify({ verdict: 'REQUEST_CHANGES', summary: 'Needs work' })
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id/result', () =>
        HttpResponse.json({ result: structured }),
      ),
    )
    render(<TaskResultViewer taskId="task-rc" />)
    await user.click(screen.getByText('Load Result'))
    await waitFor(() => {
      const badge = screen.getByTestId('verdict-badge')
      expect(badge).toHaveTextContent('REQUEST_CHANGES')
      expect(badge.className).toContain('bg-red-100')
    })
  })

  it('falls back to plain text for non-JSON result', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id/result', () =>
        HttpResponse.json({ result: 'Just plain text output' }),
      ),
    )
    render(<TaskResultViewer taskId="task-plain" />)
    await user.click(screen.getByText('Load Result'))
    await waitFor(() => {
      expect(screen.getByText('Just plain text output')).toBeInTheDocument()
    })
    // Should not show structured elements
    expect(screen.queryByTestId('verdict-badge')).not.toBeInTheDocument()
  })

  it('falls back to plain text for JSON without structured fields', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id/result', () =>
        HttpResponse.json({ result: '{"foo": "bar"}' }),
      ),
    )
    render(<TaskResultViewer taskId="task-json" />)
    await user.click(screen.getByText('Load Result'))
    await waitFor(() => {
      expect(screen.getByText('{"foo": "bar"}')).toBeInTheDocument()
    })
    expect(screen.queryByTestId('verdict-badge')).not.toBeInTheDocument()
  })

  it('renders structured result with diff section', async () => {
    const structured = JSON.stringify({
      summary: 'Changes applied',
      diff: '--- a/file.go\n+++ b/file.go\n@@ -1,2 +1,2 @@\n-old line\n+new line',
    })
    const user = userEvent.setup()
    server.use(
      http.get('/api/v1/tasks/:id/result', () =>
        HttpResponse.json({ result: structured }),
      ),
    )
    render(<TaskResultViewer taskId="task-diff" />)
    await user.click(screen.getByText('Load Result'))
    await waitFor(() => {
      expect(screen.getByTestId('diff-viewer')).toBeInTheDocument()
    })
  })
})
