import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { SubstratePoolsPanel } from './substrate-pools-panel'

const pool = {
  metadata: { name: 'warm-pool', namespace: 'default', uid: 'u1' },
  spec: {
    templateRef: { name: 'mcp-template' },
    workerPoolRef: { name: 'workers' },
    targetActors: 4,
    targetWorkers: 2,
  },
  status: {
    phase: 'Ready',
    workerCount: 2,
    actorCount: 4,
    runningActorCount: 3,
    suspendedActorCount: 1,
    actorsPerWorker: '2.0',
  },
}

describe('SubstratePoolsPanel', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('shows an empty state', async () => {
    render(<SubstratePoolsPanel />)
    await waitFor(() => expect(screen.getByText(/no substrate actor pools/i)).toBeInTheDocument())
  })

  it('lists pools with capacity and phase', async () => {
    server.use(
      http.get('/api/v1/substrate-actor-pools', () =>
        HttpResponse.json({ items: [pool], metadata: {} }),
      ),
    )
    render(<SubstratePoolsPanel />)
    await waitFor(() => expect(screen.getByText('warm-pool')).toBeInTheDocument())
    expect(screen.getByText('Ready')).toBeInTheDocument()
    expect(screen.getByText('mcp-template')).toBeInTheDocument()
    expect(screen.getByText(/3 running, 1 suspended/)).toBeInTheDocument()
  })

  it('creates a pool through the dialog', async () => {
    let posted: any
    server.use(
      http.post('/api/v1/substrate-actor-pools', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: posted.name, namespace: 'default' }, spec: posted.spec }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<SubstratePoolsPanel />)
    await user.click(screen.getByRole('button', { name: /new actor pool/i }))
    await user.type(await screen.findByLabelText('Name'), 'pool-x')
    await user.type(screen.getByLabelText('Actor template'), 'tmpl')
    await user.type(screen.getByLabelText('Target actors'), '3')
    await user.click(screen.getByRole('button', { name: /create pool/i }))
    await waitFor(() => expect(posted).toBeTruthy())
    expect(posted).toMatchObject({
      name: 'pool-x',
      namespace: 'default',
      spec: { templateRef: { name: 'tmpl' }, targetActors: 3 },
    })
  })

  it('deletes a pool after two-step confirm', async () => {
    let deleted = false
    server.use(
      http.get('/api/v1/substrate-actor-pools', () =>
        HttpResponse.json({ items: [pool], metadata: {} }),
      ),
      http.delete('/api/v1/substrate-actor-pools/:name', () => {
        deleted = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<SubstratePoolsPanel />)
    await waitFor(() => expect(screen.getByText('warm-pool')).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /^delete$/i }))
    expect(deleted).toBe(false)
    await user.click(screen.getByRole('button', { name: /confirm delete/i }))
    await waitFor(() => expect(deleted).toBe(true))
  })
})

describe('SubstratePoolsPanel edit', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('edits a pool through the prefilled dialog', async () => {
    let putBody: any
    server.use(
      http.get('/api/v1/substrate-actor-pools', () =>
        HttpResponse.json({ items: [pool], metadata: {} }),
      ),
      http.put('/api/v1/substrate-actor-pools/:name', async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json({ metadata: { name: 'warm-pool', namespace: 'default' }, spec: putBody.spec })
      }),
    )
    const user = userEvent.setup()
    render(<SubstratePoolsPanel />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Edit' })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: 'Edit' }))
    const actors = await screen.findByLabelText('Target actors')
    expect((actors as HTMLInputElement).value).toBe('4')
    await user.clear(actors)
    await user.type(actors, '6')
    await user.click(screen.getByRole('button', { name: /save changes/i }))
    await waitFor(() => expect(putBody).toBeTruthy())
    expect(putBody.spec.targetActors).toBe(6)
    expect(putBody.spec.templateRef).toEqual({ name: 'mcp-template' })
  })

  it('requires a name when creating', async () => {
    const user = userEvent.setup()
    render(<SubstratePoolsPanel />)
    await user.click(screen.getByRole('button', { name: /new actor pool/i }))
    await user.click(await screen.findByRole('button', { name: /create pool/i }))
    // Dialog stays open; nothing submitted without a name.
    expect(screen.getByLabelText('Name')).toBeInTheDocument()
  })
})
