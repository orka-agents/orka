import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { MemoryBrowser } from './memory-browser'

const sampleMemory = {
  id: 'm1',
  namespace: 'default',
  source: 'manual',
  content: 'Deploys require make manifests first',
  tags: ['infra'],
  disabled: false,
  deleted: false,
  createdAt: '2026-06-13T00:00:00Z',
  updatedAt: '2026-06-13T00:00:00Z',
  recalledCount: 3,
}

describe('MemoryBrowser', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('renders the not-enabled state on 501', async () => {
    server.use(
      http.get('/api/v1/memories', () =>
        HttpResponse.json({ error: { code: 501, message: 'memory store not configured' } }, { status: 501 }),
      ),
    )
    render(<MemoryBrowser />)
    await waitFor(() => expect(screen.getByText(/memory store is not enabled/i)).toBeInTheDocument())
  })

  it('lists memories with state and recall count', async () => {
    server.use(
      http.get('/api/v1/memories', () => HttpResponse.json({ items: [sampleMemory], metadata: {} })),
    )
    render(<MemoryBrowser />)
    await waitFor(() => expect(screen.getByText(/deploys require/i)).toBeInTheDocument())
    expect(screen.getByText('infra')).toBeInTheDocument()
    expect(screen.getByText('Active')).toBeInTheDocument()
    expect(screen.getByText('3')).toBeInTheDocument()
  })

  it('disables a memory from the row action', async () => {
    let disabledCalled = false
    server.use(
      http.get('/api/v1/memories', () => HttpResponse.json({ items: [sampleMemory], metadata: {} })),
      http.post('/api/v1/memories/:id/disable', () => {
        disabledCalled = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<MemoryBrowser />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Disable' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Disable' }))
    await waitFor(() => expect(disabledCalled).toBe(true))
  })

  it('creates a memory through the dialog', async () => {
    let posted: any
    server.use(
      http.post('/api/v1/memories', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ ...sampleMemory, id: 'm2', content: posted.content }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<MemoryBrowser />)
    await user.click(screen.getByRole('button', { name: /new memory/i }))
    await user.type(await screen.findByLabelText('Memory content'), 'kind clusters are repo-scoped')
    await user.type(screen.getByLabelText('Tags'), 'kind, infra')
    await user.click(screen.getByRole('button', { name: /create memory/i }))
    await waitFor(() => expect(posted).toBeTruthy())
    expect(posted.content).toBe('kind clusters are repo-scoped')
    expect(posted.tags).toEqual(['kind', 'infra'])
    expect(posted.source).toBe('manual')
    expect(posted.namespace).toBe('default')
  })
})

describe('MemoryBrowser edit and delete', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('edits a memory through the prefilled dialog', async () => {
    let putBody: any
    server.use(
      http.get('/api/v1/memories', () => HttpResponse.json({ items: [sampleMemory], metadata: {} })),
      http.put('/api/v1/memories/:id', async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json({ ...sampleMemory, ...putBody })
      }),
    )
    const user = userEvent.setup()
    render(<MemoryBrowser />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Edit' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Edit' }))
    const content = await screen.findByLabelText('Memory content')
    expect((content as HTMLTextAreaElement).value).toContain('Deploys require')
    await user.clear(content)
    await user.type(content, 'updated fact')
    await user.click(screen.getByRole('button', { name: /save changes/i }))
    await waitFor(() => expect(putBody).toBeTruthy())
    expect(putBody.content).toBe('updated fact')
    expect(putBody.tags).toEqual(['infra'])
  })

  it('soft-deletes a memory', async () => {
    let deleted = false
    server.use(
      http.get('/api/v1/memories', () => HttpResponse.json({ items: [sampleMemory], metadata: {} })),
      http.delete('/api/v1/memories/:id', () => {
        deleted = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<MemoryBrowser />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Delete' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(deleted).toBe(true))
  })

  it('hides row actions for deleted memories and filters via switches', async () => {
    server.use(
      http.get('/api/v1/memories', ({ request }) => {
        const url = new URL(request.url)
        if (url.searchParams.get('includeDeleted') === 'true') {
          return HttpResponse.json({
            items: [{ ...sampleMemory, id: 'm-del', deleted: true }],
            metadata: {},
          })
        }
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )
    const user = userEvent.setup()
    render(<MemoryBrowser />)
    await user.click(screen.getByRole('switch', { name: 'Include deleted' }))
    // "Deleted" also labels the filter switch; assert on the state badge.
    await waitFor(() =>
      expect(screen.getByText('Deleted', { selector: '[data-slot="badge"]' })).toBeInTheDocument(),
    )
    expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()
  })

  it('requires content when saving a new memory', async () => {
    const user = userEvent.setup()
    render(<MemoryBrowser />)
    await user.click(screen.getByRole('button', { name: /new memory/i }))
    await user.click(await screen.findByRole('button', { name: /create memory/i }))
    // Dialog stays open because validation failed client-side.
    expect(screen.getByLabelText('Memory content')).toBeInTheDocument()
  })
})
