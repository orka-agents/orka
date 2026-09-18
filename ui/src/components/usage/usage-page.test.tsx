import { beforeEach, describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import userEvent from '@testing-library/user-event'
import { render, screen, waitFor, within } from '@/test/test-utils'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { recordedTokens, type UsageReport, type UsageSummary } from '@/lib/usage'
import { UsagePage } from './usage-page'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', () => ({
  Link: ({ children, to, onClick }: { children: React.ReactNode; to: string; onClick?: () => void }) => <a href={to} onClick={onClick}>{children}</a>,
}))

function reportFixture(): UsageReport {
  const summary: UsageSummary = {
    inputTokens: 10000000, outputTokens: 2000000, totalTokens: 12000000,
    cachedInputTokens: 1000000, cacheWriteInputTokens: 0, cachedUsageReported: true, cacheWriteUsageReported: false,
    estimatedTokens: 0, measurements: 20, reportedMeasurements: 20, missingMeasurements: 0, partialMeasurements: 0,
    calls: 20, attempts: 0, completeness: 'complete', modelCost: 'Price unavailable',
    workRequests: 20, unfinishedWork: 5, prsOpened: 15, prsReady: 2, prsMerged: 10, prsClosed: 0, prsAssisted: 0,
    tokensPerPROpened: 800000, tokensPerPRMerged: 1200000,
  }
  return {
    selection: { teams: ['payments'], from: '2026-09-01T00:00:00Z', until: '2026-10-01T00:00:00Z', asOf: '2026-10-15T12:00:00Z' },
    summary, teams: [{ namespace: 'payments', summary }], otherWork: [],
    works: Array.from({ length: 20 }, (_, i) => {
      const usage = { ...summary, inputTokens: 500000, outputTokens: 100000, totalTokens: 600000, measurements: 1, reportedMeasurements: 1, calls: 1 }
      return {
        id: `work-${i}`, namespace: 'payments', monitorName: 'monitor', repository: 'org/repo', kind: 'issue', number: i + 1,
        startedAt: '2026-09-05T12:00:00Z', models: ['served-model'], summary: { ...usage, workRequests: 1, prsOpened: i < 15 ? 1 : 0, prsMerged: i < 10 ? 1 : 0 },
        pullRequests: i < 15 ? [{ namespace: 'payments', repository: 'org/repo', number: i + 101, url: `https://github.com/org/repo/pull/${i + 101}`,
          state: i < 10 ? 'merged' : 'open', origin: 'created', ready: false, headSHA: 'head-one', observedAt: '2026-10-15T12:00:00Z' }] : [],
        tasks: [{ namespace: 'payments', taskUID: `task-${i}`, taskName: `task-${i}`, phase: i < 15 ? 'Succeeded' : 'Failed',
          role: 'implementation', sessionName: 'conversation', shared: false, startedAt: '2026-09-05T12:00:00Z', usage,
          measurements: [{ id: `call-${i}`, scope: 'call', source: 'provider', provider: 'openai', model: 'served-model', inputTokens: 500000,
            outputTokens: 100000, cachedInputTokens: 50000, cacheWriteInputTokens: null, status: 'completed', completeness: 'complete', observedAt: '2026-09-05T13:00:00Z' }] }],
      }
    }),
  }
}

describe('UsagePage', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'payments' })
    useAuthStore.setState({ token: 'fixture' })
  })

  it('shows the Payments ratios and the linked request evidence', async () => {
    server.use(http.get('/api/v1/usage', () => HttpResponse.json(reportFixture())))
    const user = userEvent.setup()
    render(<UsagePage />)
    const heading = await screen.findByText('Tokens per merged PR')
    const card = heading.closest('[data-slot="card"]') as HTMLElement
    expect(within(card).getByText('1,200,000')).toBeInTheDocument()
    expect(within(card).getByText('800,000')).toBeInTheDocument()
    expect(within(card).getByText('Price unavailable')).toBeInTheDocument()
    expect(screen.getByText(/including unsuccessful work/)).toBeInTheDocument()
    await user.click(screen.getByText('org/repo #1'))
    expect(screen.getByRole('link', { name: 'PR #101' })).toHaveAttribute('href', 'https://github.com/org/repo/pull/101')
    await user.click(screen.getByText('task-0', { exact: false, selector: 'summary' }))
    const task = screen.getByRole('link', { name: 'Open Task task-0' }).closest('details') as HTMLElement
    expect(within(task).getByText('Input 500,000 · Output 100,000 · Cached reads 50,000 · Cached writes Unavailable')).toBeInTheDocument()
  })

  it('explains zero merges and missing usage without showing a free call', async () => {
    const report = reportFixture()
    report.summary = { ...report.summary, prsMerged: 0, tokensPerPRMerged: null, reportedMeasurements: 0, missingMeasurements: 20, completeness: 'unavailable' }
    report.teams = [{ namespace: 'payments', summary: report.summary }]
    report.works = []
    server.use(http.get('/api/v1/usage', () => HttpResponse.json(report)))
    render(<UsagePage />)
    expect(await screen.findByText('No PRs merged yet')).toBeInTheDocument()
    expect(screen.getByText(/0 of 20 measurements contain usage/)).toBeInTheDocument()
    expect(screen.getAllByText(/Usage unavailable/).length).toBeGreaterThan(0)
    expect(recordedTokens({ ...report.summary, measurements: 0 })).toBe('No model calls')
    expect(recordedTokens({ ...report.summary, completeness: 'complete', totalTokens: 0 })).toBe('0')
  })

  it('submits team, repository, model and work-type filters together', async () => {
    const requests: URL[] = []
    server.use(http.get('/api/v1/usage', ({ request }) => { requests.push(new URL(request.url)); return HttpResponse.json(reportFixture()) }))
    const user = userEvent.setup()
    render(<UsagePage />)
    await screen.findByText('Tokens per merged PR')
    await user.type(screen.getByLabelText('Teams'), 'payments,inventory')
    await user.type(screen.getByLabelText('Repository'), 'org/repo')
    await user.type(screen.getByLabelText('Model used by the request'), 'served-model')
    await user.selectOptions(screen.getByLabelText('Work type'), 'pull_request')
    await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    await waitFor(() => expect(requests).toHaveLength(2))
    const params = requests[1].searchParams
    expect(params.get('namespace')).toBe('payments')
    expect(params.get('teams')).toBe('payments,inventory')
    expect(params.get('repository')).toBe('org/repo')
    expect(params.get('model')).toBe('served-model')
    expect(params.get('kind')).toBe('pull_request')
  })

  it('shows denied access as an error', async () => {
    server.use(http.get('/api/v1/usage', () => HttpResponse.json({ error: { message: 'not authorized' } }, { status: 403 })))
    render(<UsagePage />)
    expect(await screen.findByRole('alert')).toHaveTextContent('not authorized')
    expect(screen.queryByText('No PRs merged yet')).not.toBeInTheDocument()
  })
})
