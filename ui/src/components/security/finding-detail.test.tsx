import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

// A fixed query result for cases the real hook would reach only after its
// retries (a 500) or a background refetch; null uses the real hook.
const findingQuery = vi.hoisted(() => ({ current: null as null | Record<string, unknown> }))

vi.mock('@/hooks/use-security', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/hooks/use-security')>()
  return { ...actual, useFinding: (id: string) => findingQuery.current ?? actual.useFinding(id) }
})

import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import userEvent from '@testing-library/user-event'
import { render, screen, waitFor } from '@/test/test-utils'
import { ApiError } from '@/lib/api-client'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import type { SecurityFinding } from '@/schemas/security'
import { FindingDetail } from './finding-detail'

const finding: SecurityFinding = {
  id: 'f-1',
  namespace: 'default',
  repositoryScan: 'payments-main',
  fingerprint: 'fp-1',
  title: 'SQL injection in search',
  summary: 'User input reaches the query unescaped.',
  severity: 'high',
  confidence: 'high',
  validationStatus: 'pending',
  state: 'open',
}

describe('FindingDetail', () => {
  beforeEach(() => {
    findingQuery.current = null
    document.title = 'Orka'
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    server.use(http.get('/api/v1/security/findings/:id/patches', () => HttpResponse.json({ items: [] })))
  })

  it('renders a titled not-found page and sends no patch request for a missing finding', async () => {
    const requests: string[] = []
    server.use(http.all('/api/v1/security/*', ({ request }) => {
      requests.push(new URL(request.url).pathname)
      return HttpResponse.json({ error: { code: 404, message: 'finding not found' } }, { status: 404 })
    }))

    render(<FindingDetail findingId="f-404" />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Finding not found' })).toBeInTheDocument()
    expect(screen.getByText('No finding with ID "f-404" in default')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to Security' })).toHaveAttribute('href', '/security')
    expect(document.title).toBe('Finding not found · Orka')
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(requests).toEqual(['/api/v1/security/findings/f-404'])
  })

  it('says the token cannot read the finding instead of "not found" on 403', async () => {
    server.use(http.get('/api/v1/security/findings/f-1', () =>
      HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })))

    render(<FindingDetail findingId="f-1" />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Could not load finding' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('Not authorized to view finding f-1')
    expect(screen.getByRole('link', { name: 'Back to Security' })).toHaveAttribute('href', '/security')
    expect(document.title).toBe('Could not load finding · Orka')
    expect(screen.queryByText('Finding not found')).not.toBeInTheDocument()
  })

  it('drops cached data once the finding is forbidden', () => {
    findingQuery.current = { isLoading: false, data: finding, error: new ApiError(403, '{"error":{"code":403,"message":"not authorized"}}') }

    render(<FindingDetail findingId="f-1" />)

    expect(screen.getByRole('heading', { level: 1, name: 'Could not load finding' })).toBeInTheDocument()
    expect(screen.queryByText('SQL injection in search')).not.toBeInTheDocument()
  })

  it('shows a load error, not "not found", when the finding request fails', () => {
    findingQuery.current = { isLoading: false, data: undefined, error: new ApiError(500, '{"error":{"code":500,"message":"security store unavailable"}}') }

    render(<FindingDetail findingId="f-1" />)

    expect(screen.getByRole('heading', { level: 1, name: 'Could not load finding' })).toBeInTheDocument()
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Could not load finding f-1')
    expect(alert).toHaveTextContent('security store unavailable')
    expect(screen.queryByText('Finding not found')).not.toBeInTheDocument()
  })

  it('downloads evidence artifacts with the bearer token instead of linking to the API', async () => {
    useAuthStore.setState({ token: 'test-token' })
    let downloadUrl = ''
    let auth: string | null = null
    server.use(http.get('/api/v1/tasks/:id/artifacts/:file', ({ request }) => {
      downloadUrl = request.url
      auth = request.headers.get('authorization')
      return HttpResponse.text('{}')
    }))
    // jsdom lacks blob URL + anchor click plumbing; stub them.
    const createUrl = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:x')
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    findingQuery.current = {
      isLoading: false,
      error: null,
      data: { ...finding, evidence: [{ kind: 'artifact', taskName: 'scan task', name: 'semgrep report.json', label: 'Semgrep report' }] },
    }

    try {
      render(<FindingDetail findingId="f-1" />)
      expect(screen.queryByRole('link', { name: 'Semgrep report' })).not.toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Semgrep report' }))

      await waitFor(() => expect(downloadUrl).toContain('/api/v1/tasks/scan%20task/artifacts/semgrep%20report.json?namespace=default'))
      expect(auth).toBe('Bearer test-token')
    } finally {
      createUrl.mockRestore(); revoke.mockRestore(); click.mockRestore()
    }
  })

  it('keeps line breaks in evidence quotes', () => {
    findingQuery.current = {
      isLoading: false,
      error: null,
      data: { ...finding, evidence: [{ kind: 'code', path: 'internal/search.go', startLine: 40, quote: 'q := "SELECT *"\nrows, _ := db.Query(q + term)' }] },
    }

    render(<FindingDetail findingId="f-1" />)

    expect(screen.getByText(/rows, _ := db.Query/)).toHaveClass('whitespace-pre-wrap')
  })

  it('keeps showing a loaded finding when a refetch fails', () => {
    findingQuery.current = { isLoading: false, data: finding, error: new ApiError(500, '{"error":{"code":500,"message":"security store unavailable"}}') }

    render(<FindingDetail findingId="f-1" />)

    expect(screen.getByRole('heading', { level: 1, name: 'SQL injection in search' })).toBeInTheDocument()
    expect(screen.queryByText('Could not load finding')).not.toBeInTheDocument()
  })
})
