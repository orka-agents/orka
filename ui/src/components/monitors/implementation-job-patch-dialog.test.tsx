import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { ImplementationJobPatchDialog } from './implementation-job-patch-dialog'
import type { MonitorImplementationJob } from '@/schemas/monitor'

const job = { id: 'job-1', issueNumber: 42 } as MonitorImplementationJob

describe('ImplementationJobPatchDialog', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('shows the patch artifact when available', async () => {
    server.use(
      http.get('/api/v1/monitors/implementation-jobs/:id/patch-preview', () =>
        HttpResponse.json({ job, patch: { files: [{ path: 'main.go' }] }, contentType: 'application/json' }),
      ),
    )
    const user = userEvent.setup()
    render(<ImplementationJobPatchDialog job={job} />)
    await user.click(screen.getByRole('button', { name: /preview patch/i }))
    await waitFor(() => expect(screen.getByText(/main\.go/)).toBeInTheDocument())
  })

  it('explains 501 as a missing artifact store', async () => {
    server.use(
      http.get('/api/v1/monitors/implementation-jobs/:id/patch-preview', () =>
        HttpResponse.json({ error: { code: 501, message: 'no artifact store' } }, { status: 501 }),
      ),
    )
    const user = userEvent.setup()
    render(<ImplementationJobPatchDialog job={job} />)
    await user.click(screen.getByRole('button', { name: /preview patch/i }))
    await waitFor(() => expect(screen.getByText(/no artifact store configured/i)).toBeInTheDocument())
  })

  it('explains 404 as no patch artifact yet', async () => {
    server.use(
      http.get('/api/v1/monitors/implementation-jobs/:id/patch-preview', () =>
        HttpResponse.json({ error: { code: 404, message: 'not found' } }, { status: 404 }),
      ),
    )
    const user = userEvent.setup()
    render(<ImplementationJobPatchDialog job={job} />)
    await user.click(screen.getByRole('button', { name: /preview patch/i }))
    await waitFor(() => expect(screen.getByText(/has not produced a patch artifact/i)).toBeInTheDocument())
  })
})
