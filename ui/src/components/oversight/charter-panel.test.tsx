import { describe, expect, it, beforeEach, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen, within } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { CharterPanel } from './charter-panel'
import { charterConfig } from '@/test/fixtures/oversight'

describe('CharterPanel', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('lists declared bounds verbatim, highlighted first, with counts and pivots', async () => {
    const queries: string[] = []
    server.use(
      http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)),
      http.get('/api/v1/oversight/decisions/groups', ({ request }) => {
        const url = new URL(request.url)
        queries.push(`${url.searchParams.get('q')}@${url.searchParams.get('window')}`)
        const searched = url.searchParams.get('q') === 'charter:charter-r1 bound:B1' ? 3 : 0
        return HttpResponse.json({ namespace: 'default', by: 'agent', groups: [], searched })
      }),
    )
    const onPivot = vi.fn()
    const onOpenBound = vi.fn()
    render(<CharterPanel window="24h" onPivot={onPivot} onOpenBound={onOpenBound} />)
    const rules = await screen.findByRole('list', { name: 'Rules of Workspace charter' })
    const items = within(rules).getAllByRole('listitem')
    expect(items.map((li) => li.querySelector('span')?.textContent)).toEqual(['Rule 3', 'Rule 1', 'Rule 2'])
    expect(items[0]).toHaveAttribute('data-highlight', 'true')
    expect(within(items[0]).getByText('Never send workspace data to undeclared hosts.')).toBeInTheDocument()
    expect(screen.getByText('Demo charter: preview rule check, audit only')).toBeInTheDocument()
    expect(await screen.findByTestId('bound-count-B1')).toHaveTextContent('3 actions broke it in the last 24h')
    expect(await screen.findByTestId('bound-count-B0')).toHaveTextContent('0 actions broke it')
    expect(queries).toContain('charter:charter-r1 bound:B1@24h')
    // Bound ids stay out of the visible list; the count opens the matching actions.
    expect(rules).not.toHaveTextContent(/B0|B1|B2/)
    expect(within(items[0]).getByText('Rule 3')).toHaveAttribute('title', 'Never send workspace data to undeclared hosts.')
    expect(screen.getByText('audit mode · 3 rules')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Show the 3 actions that broke rule 2 in the last 24h' }))
    expect(onOpenBound).toHaveBeenCalledWith('charter:charter-r1 bound:B1')
    expect(onPivot).not.toHaveBeenCalled()
  })

  it('labels a non-preview charter watched, not enforced, unless the config lists it as a demo', async () => {
    const charter = { ...charterConfig.charters![0], preview: false }
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json({ ...charterConfig, charters: [charter] })))
    const { unmount } = render(<CharterPanel onPivot={() => {}} />)
    expect(await screen.findByText('Watched, not enforced')).toBeInTheDocument()
    unmount()
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json({ ...charterConfig, demoCharterRevisions: ['charter-r1'], charters: [charter] })))
    render(<CharterPanel onPivot={() => {}} />)
    expect(await screen.findByText('Demo charter: preview rule check, audit only')).toBeInTheDocument()
  })

  it('shows a quiet state when no charter is declared', async () => {
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json({ queryKeys: [], maxBulkFeedback: 200, demoCharterRevisions: [], charters: [] })))
    render(<CharterPanel onPivot={() => {}} />)
    expect(await screen.findByText('No charter declared.')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
