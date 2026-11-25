import { describe, expect, it, beforeEach, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
const mockNavigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, useNavigate: () => mockNavigate }
})

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { TaskOversightTab } from './task-oversight-tab'
import { makeDecision, riskyDecision } from '@/test/fixtures/oversight'

describe('TaskOversightTab', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    mockNavigate.mockClear()
    server.use(
      http.get('/api/v1/oversight/decisions', () => HttpResponse.json({ namespace: 'default', items: [riskyDecision, makeDecision()] })),
      http.get('/api/v1/oversight/decisions/:id', () => HttpResponse.json(riskyDecision)),
    )
  })

  it("summarises the Task's taint, coverage and charter and opens the drawer", async () => {
    render(<TaskOversightTab taskName="build-1" />)
    const taint = await screen.findByRole('group', { name: 'Task taint' })
    expect(within(taint).getByText('untrusted_input')).toBeInTheDocument()
    expect(within(taint).getByText('parent_web')).toBeInTheDocument()
    const coverage = screen.getByRole('group', { name: 'Task coverage' })
    expect(coverage).toHaveTextContent('2 of 2 actions attributed')
    expect(within(coverage).getByText('dns_over_https')).toBeInTheDocument()
    expect(screen.getByRole('group', { name: 'Task charter' })).toHaveTextContent('Charter charter-r1 (watched, not enforced)')
    await userEvent.click(screen.getByRole('button', { name: /sent 2 KB to cdn.registry.example:443/ }))
    await userEvent.click(await screen.findByRole('button', { name: 'Details' }))
    const effects = await screen.findByRole('region', { name: 'Kernel-observed effects' })
    await userEvent.click(within(effects).getAllByRole('button', { name: 'Search all time for bin:wget' })[0])
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/oversight', search: { q: 'bin:wget' } })
  })
})
