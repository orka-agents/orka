import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@/test/test-utils'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import * as connectors from '@/lib/connectors'
import { ConnectorsPage } from './connectors-page'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

const API = '/api/v1'
const github = {
  name: 'github', namespace: 'orka-system', displayName: 'GitHub', ready: true,
  tools: [{ name: 'list_pull_requests', class: 'read' }, { name: 'create_pull_request', class: 'write' }],
}
const jira = { name: 'jira', namespace: 'orka-system', displayName: 'Jira', ready: false, tools: [{ name: 'jira_search', class: 'read' }] }
const linked = { name: 'github-abc', namespace: 'orka-system', provider: 'github', mode: 'readOnly', state: 'Ready', ready: true, linkedAt: '2026-09-28T10:00:00Z' }

function useProviders(items: unknown[], connections: unknown[] = []) {
  server.use(
    http.get(`${API}/connectors`, () => HttpResponse.json({ items })),
    http.get(`${API}/connections`, () => HttpResponse.json({ items: connections })),
  )
}

describe('ConnectorsPage', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'orka-system', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    window.history.replaceState(null, '', '/settings/connectors')
  })

  it('lists providers with link state and starts consent', async () => {
    useProviders([github, jira], [linked])
    const open = vi.spyOn(connectors, 'openAuthorizeURL').mockImplementation(() => {})
    let posted: unknown
    server.use(http.post(`${API}/connections`, async ({ request }) => {
      posted = await request.json()
      return HttpResponse.json({ connection: { ...linked, provider: 'jira', name: 'jira-x', state: 'Pending', ready: false }, authorizeURL: 'https://jira.example.test/authorize?state=s' })
    }))
    render(<ConnectorsPage />)
    await waitFor(() => expect(screen.getByText('GitHub')).toBeInTheDocument())
    expect(screen.getByText('Linked · read only')).toBeInTheDocument()
    expect(screen.getByText('Unavailable')).toBeInTheDocument()
    expect(screen.getByText(/create_pull_request/)).toBeInTheDocument()
    // A linked provider offers mode change and disconnect, not connect.
    expect(screen.getByRole('button', { name: 'Allow writes' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument()
    // An unready provider cannot be connected yet.
    expect(screen.getByRole('button', { name: 'Connect (read only)' })).toBeDisabled()
    open.mockRestore()
    expect(posted).toBeUndefined()
  })

  it('starts consent for a ready provider and sends the browser to the provider', async () => {
    useProviders([github])
    const open = vi.spyOn(connectors, 'openAuthorizeURL').mockImplementation(() => {})
    let posted: Record<string, unknown> | undefined
    server.use(http.post(`${API}/connections`, async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>
      return HttpResponse.json({ connection: { ...linked, state: 'Pending', ready: false, mode: 'readWrite' }, authorizeURL: 'https://github.com/login/oauth/authorize?state=s' })
    }))
    render(<ConnectorsPage />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Connect with writes' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: 'Connect with writes' }))
    await waitFor(() => expect(open).toHaveBeenCalledWith('https://github.com/login/oauth/authorize?state=s'))
    expect(posted).toEqual({ provider: 'github', mode: 'readWrite' })
    open.mockRestore()
  })

  it('completes a consent from the callback fragment exactly once, in the sealed namespace', async () => {
    useProviders([github], [{ ...linked, state: 'Pending', ready: false }])
    window.history.replaceState(null, '', '/settings/connectors?status=pending&connection=github-abc&namespace=team-a#completion=one-time')
    const completions: { body: unknown; namespace: string | null }[] = []
    server.use(http.post(`${API}/connections/github-abc/complete`, async ({ request }) => {
      completions.push({ body: await request.json(), namespace: new URL(request.url).searchParams.get('namespace') })
      return HttpResponse.json({ ...linked, mode: 'readWrite' })
    }))
    render(<ConnectorsPage search={{ status: 'pending', connection: 'github-abc', namespace: 'team-a' }} />)
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Linked github (readWrite).'))
    // The page shows orka-system, but the consent was sealed in team-a.
    expect(completions).toEqual([{ body: { completion: 'one-time' }, namespace: 'team-a' }])
    expect(window.location.hash).toBe('')
  })

  it('keeps the completion token for a retry when finishing fails', async () => {
    useProviders([github], [{ ...linked, state: 'Pending', ready: false }])
    window.history.replaceState(null, '', '/settings/connectors?status=pending&connection=github-abc#completion=one-time')
    let attempts = 0
    server.use(http.post(`${API}/connections/github-abc/complete`, async () => {
      attempts += 1
      if (attempts === 1) return new HttpResponse('not signed in', { status: 401 })
      return HttpResponse.json({ ...linked, mode: 'readWrite' })
    }))
    render(<ConnectorsPage search={{ status: 'pending', connection: 'github-abc' }} />)
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('orka connection complete github-abc'))
    // The token is still in the address bar, so a reload could retry too.
    expect(window.location.hash).toBe('#completion=one-time')
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Linked github (readWrite).'))
    expect(attempts).toBe(2)
    expect(window.location.hash).toBe('')
  })

  it('explains a failed callback', async () => {
    useProviders([github])
    render(<ConnectorsPage search={{ status: 'error', reason: 'scopes_denied' }} />)
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('fewer permissions'))
  })

  it('tells a non-person token there is nothing to link', async () => {
    server.use(
      http.get(`${API}/connectors`, () => new HttpResponse('connector endpoints require a verified person', { status: 403 })),
      http.get(`${API}/connections`, () => new HttpResponse('connector endpoints require a verified person', { status: 403 })),
    )
    render(<ConnectorsPage />)
    await waitFor(() => expect(screen.getByText('Sign in as yourself to link accounts')).toBeInTheDocument())
  })

  it('disconnects a linked account', async () => {
    useProviders([github], [linked])
    let deleted = ''
    server.use(http.delete(`${API}/connections/:name`, ({ params }) => {
      deleted = String(params.name)
      return new HttpResponse(null, { status: 204 })
    }))
    render(<ConnectorsPage />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Disconnect' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: 'Disconnect' }))
    await waitFor(() => expect(deleted).toBe('github-abc'))
  })
})
