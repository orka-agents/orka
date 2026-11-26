import { describe, expect, it, beforeEach, vi } from 'vitest'
import { render, screen, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { ProducersPanel } from './producers-panel'
import { producersResponse } from '@/test/fixtures/oversight'

describe('ProducersPanel', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('shows declared identity, unverified trust and every metric with its run and sample size', async () => {
    server.use(http.get('/api/v1/oversight/producers', () => HttpResponse.json(producersResponse)))
    render(<ProducersPanel />)
    const rows = await screen.findAllByTestId('producer-row')
    expect(within(rows[0]).getByText('Scorer Alpha')).toBeInTheDocument()
    expect(within(rows[0]).getByText('(declared, not verified by Orka)')).toBeInTheDocument()
    expect(within(rows[0]).getByText('0.92 AUROC · run r1 · n=26')).toBeInTheDocument()
    expect(within(rows[0]).getByText('Small sample: n=26')).toBeInTheDocument()
    expect(within(rows[1]).getByText('scorer-b')).toBeInTheDocument()
    expect(within(rows[1]).getByText(/Last 24 hours: 10 evaluations, 2 failures/)).toBeInTheDocument()
  })

  it.each([404, 501])('reads a %s as no producer declarations', async (status) => {
    server.use(http.get('/api/v1/oversight/producers', () => HttpResponse.json({ error: 'x' }, { status })))
    render(<ProducersPanel />)
    expect(await screen.findByText('No producer declarations.')).toBeInTheDocument()
  })
})
