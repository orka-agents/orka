import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { MemoryPage } from './memory-page'

describe('MemoryPage', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('renders both tabs and the governance description', async () => {
    render(<MemoryPage />)
    expect(screen.getByRole('heading', { name: 'Memory' })).toBeInTheDocument()
    expect(screen.getByText(/governance-first/i)).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: /memories/i })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: /proposals/i })).toBeInTheDocument()
  })

  it('shows a pending count chip on the proposals tab', async () => {
    server.use(
      http.get('/api/v1/memory-proposals', ({ request }) => {
        const url = new URL(request.url)
        if (url.searchParams.get('status') === 'pending') {
          return HttpResponse.json({
            items: [
              {
                id: 'p1',
                namespace: 'default',
                type: 'memory',
                title: 'One pending',
                status: 'pending',
                createdAt: '2026-06-13T00:00:00Z',
                updatedAt: '2026-06-13T00:00:00Z',
              },
            ],
            metadata: {},
          })
        }
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )
    render(<MemoryPage />)
    await waitFor(() => expect(screen.getByRole('tab', { name: /proposals/i })).toHaveTextContent('1'))
  })

  it('switches to the proposals tab', async () => {
    const user = userEvent.setup()
    render(<MemoryPage />)
    await user.click(screen.getByRole('tab', { name: /proposals/i }))
    expect(await screen.findByRole('group', { name: /filter by status/i })).toBeInTheDocument()
  })
})
