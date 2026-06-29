import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskArtifactsPanel } from './task-artifacts-panel'

describe('TaskArtifactsPanel', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('lists artifacts with size and content type', async () => {
    server.use(http.get('/api/v1/tasks/:id/artifacts', () => HttpResponse.json({
      artifacts: [
        { filename: 'report.json', contentType: 'application/json', size: 2048 },
        { filename: 'log.txt', contentType: 'text/plain', size: 512 },
      ],
    })))
    render(<TaskArtifactsPanel taskId="task-1" />)
    await waitFor(() => {
      expect(screen.getByText('report.json')).toBeInTheDocument()
    })
    expect(screen.getByText('log.txt')).toBeInTheDocument()
    expect(screen.getByText('2.0 KB')).toBeInTheDocument()
    expect(screen.getByText('512 B')).toBeInTheDocument()
    expect(screen.getByText('application/json')).toBeInTheDocument()
  })

  it('shows empty state when no artifacts', async () => {
    server.use(http.get('/api/v1/tasks/:id/artifacts', () => HttpResponse.json({ artifacts: [] })))
    render(<TaskArtifactsPanel taskId="task-1" />)
    await waitFor(() => {
      expect(screen.getByText('No artifacts')).toBeInTheDocument()
    })
    expect(screen.getByText(/Outputs uploaded by this task appear here\./)).toBeInTheDocument()
  })

  it('distinguishes a backend failure from an empty result', async () => {
    const { ApiError } = await import('@/lib/api-client')
    const mod = await import('@/hooks/use-task-artifacts')
    const spy = vi.spyOn(mod, 'useTaskArtifacts').mockReturnValue({
      data: undefined, isLoading: false, error: new ApiError(403, 'forbidden'),
    } as ReturnType<typeof mod.useTaskArtifacts>)
    render(<TaskArtifactsPanel taskId="task-1" />)
    expect(screen.getByText(/load artifacts/i)).toBeInTheDocument()
    expect(screen.queryByText('No artifacts')).not.toBeInTheDocument()
    spy.mockRestore()
  })

  it('downloads via authenticated fetch using the encoded URL', async () => {
    let downloadUrl = ''
    let auth: string | null = null
    server.use(
      http.get('/api/v1/tasks/:id/artifacts', () => HttpResponse.json({
        artifacts: [{ filename: 'my report.json', contentType: 'application/json', size: 100 }],
      })),
      http.get('/api/v1/tasks/:id/artifacts/:file', ({ request }) => {
        downloadUrl = request.url
        auth = request.headers.get('authorization')
        return HttpResponse.text('blob')
      }),
    )
    // jsdom lacks blob URL + anchor click plumbing; stub them.
    const createUrl = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:x')
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    render(<TaskArtifactsPanel taskId="task-1" />)
    await waitFor(() => screen.getByText('my report.json'))
    await userEvent.click(screen.getByRole('button', { name: /Download/ }))
    await waitFor(() => expect(downloadUrl).toContain('my%20report.json'))
    expect(auth).toBe('Bearer test-token')
    createUrl.mockRestore(); revoke.mockRestore()
  })
})
