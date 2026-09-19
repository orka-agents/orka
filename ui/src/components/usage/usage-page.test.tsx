import { beforeEach, describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import userEvent from '@testing-library/user-event'
import { act, render, screen, waitFor, within } from '@/test/test-utils'
import { server } from '@/test/mocks/server'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { recordedTokens, type UsageReport, type UsageSummary, type UsageWork } from '@/lib/usage'
import { UsagePage } from './usage-page'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))
vi.mock('@tanstack/react-router', () => ({
  Link: ({ children, to, onClick }: { children: React.ReactNode; to: string; onClick?: () => void }) => <a href={to} onClick={onClick}>{children}</a>,
}))

function reportFixture(): UsageReport & { works: UsageWork[] } {
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
    page: { limit: 25, offset: 0, total: 20 },
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
    const report = reportFixture()
    const requests: URL[] = []
    server.use(
      http.get('/api/v1/usage', () => HttpResponse.json(report)),
      http.get('/api/v1/usage/work/:id', ({ request, params }) => {
        requests.push(new URL(request.url))
        return HttpResponse.json({ work: report.works.find((work) => work.id === params.id) })
      }),
    )
    const user = userEvent.setup()
    render(<UsagePage />)
    const heading = await screen.findByText('Tokens per merged PR')
    const card = heading.closest('[data-slot="card"]') as HTMLElement
    expect(within(card).getByText('1,200,000')).toBeInTheDocument()
    expect(within(card).getByText('800,000')).toBeInTheDocument()
    expect(within(card).getByText('Price unavailable')).toBeInTheDocument()
    expect(screen.getByText(/including unsuccessful work/)).toBeInTheDocument()
    expect(requests).toHaveLength(0)
    expect(screen.queryByText('Models: served-model')).not.toBeInTheDocument()
    expect(screen.queryByText(/Attempt call-0/)).not.toBeInTheDocument()
    await user.click(screen.getByText('org/repo #1'))
    expect(await screen.findByRole('link', { name: 'PR #101' })).toHaveAttribute('href', 'https://github.com/org/repo/pull/101')
    expect(requests).toHaveLength(1)
    expect(requests[0].searchParams.get('asOf')).toBe(report.selection.asOf)
    expect(screen.queryByText(/Attempt call-0/)).not.toBeInTheDocument()
    await user.click(screen.getByText('task-0', { exact: false, selector: 'summary' }))
    const task = (await screen.findByRole('link', { name: 'Open Task task-0' })).closest('details') as HTMLElement
    expect(within(task).getByText('Input 500,000 · Output 100,000 · Cached reads 50,000 · Cached writes Unavailable')).toBeInTheDocument()
  })

  it('pins the report time when paging work summaries', async () => {
    const report = reportFixture()
    const requests: URL[] = []
    server.use(http.get('/api/v1/usage', ({ request }) => {
      const url = new URL(request.url)
      requests.push(url)
      const offset = Number(url.searchParams.get('offset'))
      return HttpResponse.json({ ...report, page: { limit: 25, offset, total: 26 },
        works: [{ ...report.works[0], id: `work-${offset}`, number: offset + 1 }] })
    }))
    const user = userEvent.setup()
    render(<UsagePage />)
    await screen.findByText('org/repo #1')
    await user.click(screen.getByRole('button', { name: 'Next Work requests' }))
    await screen.findByText('org/repo #26')
    expect(screen.queryByText('org/repo #1')).not.toBeInTheDocument()
    expect(screen.getAllByText('1,200,000')).toHaveLength(2)
    expect(requests[1].searchParams.get('offset')).toBe('25')
    expect(requests[1].searchParams.get('asOf')).toBe(report.selection.asOf)
    await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    await screen.findByText('org/repo #1')
    await waitFor(() => expect(requests).toHaveLength(3))
    expect(requests.at(-1)?.searchParams.get('asOf')).toBeNull()
  })

  it('fetches other usage only on expansion and pages its Tasks', async () => {
    const report = reportFixture()
    const group = { category: 'unassociated', explanation: 'Usage without a work reference', usage: report.summary, taskCount: 26 }
    report.otherWork = [group]
    const requests: URL[] = []
    server.use(
      http.get('/api/v1/usage', () => HttpResponse.json(report)),
      http.get('/api/v1/usage/other/unassociated', ({ request }) => {
        const url = new URL(request.url)
        requests.push(url)
        const offset = Number(url.searchParams.get('offset'))
        return HttpResponse.json({ otherWork: { ...group, page: { offset, limit: 25, total: 26 },
          tasks: [{ ...report.works[0].tasks![0], taskUID: `chat-${offset}`, taskName: `chat-${offset}` }] } })
      }),
    )
    const user = userEvent.setup()
    render(<UsagePage />)
    await screen.findByText('Tokens per merged PR')
    expect(requests).toHaveLength(0)
    await user.click(screen.getByText('Chat and usage without a work reference'))
    await screen.findByText('chat-0', { exact: false, selector: 'summary' })
    expect(requests[0].searchParams.get('asOf')).toBe(report.selection.asOf)
    expect(requests[0].searchParams.get('teams')).toBe('payments')
    await user.click(screen.getByRole('button', { name: 'Next Chat and usage without a work reference Tasks' }))
    await screen.findByText('chat-25', { exact: false, selector: 'summary' })
    expect(screen.queryByText('chat-0', { exact: false, selector: 'summary' })).not.toBeInTheDocument()
    expect(requests[1].searchParams.get('offset')).toBe('25')
  })

  it('keeps other usage pagination on one report snapshot until filters change', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    try {
      const report = reportFixture()
      const group = { category: 'unassociated', explanation: 'Usage without a work reference', usage: report.summary, taskCount: 51 }
      report.otherWork = [group]
      const summaryRequests: URL[] = []
      const detailRequests: URL[] = []
      server.use(
        http.get('/api/v1/usage', ({ request }) => {
          const url = new URL(request.url)
          summaryRequests.push(url)
          return HttpResponse.json(report)
        }),
        http.get('/api/v1/usage/other/unassociated', ({ request }) => {
          const url = new URL(request.url)
          detailRequests.push(url)
          const offset = Number(url.searchParams.get('offset'))
          return HttpResponse.json({ otherWork: { ...group, page: { offset, limit: 25, total: 51 },
            tasks: [{ ...report.works[0].tasks![0], taskUID: `chat-${offset}`, taskName: `chat-${offset}` }] } })
        }),
      )
      const user = userEvent.setup()
      render(<UsagePage />)
      await user.click(await screen.findByText('Chat and usage without a work reference'))
      await screen.findByText('chat-0', { exact: false, selector: 'summary' })
      await user.click(screen.getByRole('button', { name: 'Next Chat and usage without a work reference Tasks' }))
      await screen.findByText('chat-25', { exact: false, selector: 'summary' })
      await waitFor(() => expect(summaryRequests).toHaveLength(2))
      expect(summaryRequests[1].searchParams.get('asOf')).toBe(report.selection.asOf)
      expect(summaryRequests[1].searchParams.get('offset')).toBe('0')

      await act(async () => { await vi.advanceTimersByTimeAsync(60000) })
      expect(summaryRequests).toHaveLength(2)
      expect(screen.getByText('chat-25', { exact: false, selector: 'summary' })).toBeInTheDocument()
      await user.click(screen.getByRole('button', { name: 'Next Chat and usage without a work reference Tasks' }))
      await screen.findByText('chat-50', { exact: false, selector: 'summary' })
      expect(detailRequests.map((request) => request.searchParams.get('offset'))).toEqual(['0', '25', '50'])
      expect(detailRequests.every((request) => request.searchParams.get('asOf') === report.selection.asOf)).toBe(true)

      await user.type(screen.getByLabelText('Model used by the request'), 'served-model')
      await user.click(screen.getByRole('button', { name: 'Apply filters' }))
      await waitFor(() => expect(summaryRequests).toHaveLength(3))
      expect(summaryRequests.at(-1)?.searchParams.get('asOf')).toBeNull()
      expect(screen.queryByText('chat-50', { exact: false, selector: 'summary' })).not.toBeInTheDocument()
      await act(async () => { await vi.advanceTimersByTimeAsync(60000) })
      await waitFor(() => expect(summaryRequests).toHaveLength(4))
    } finally {
      vi.useRealTimers()
    }
  })

  it.each([
    { group: 'work', page: 'Pull requests', item: 'PR #126' },
    { group: 'work', page: 'Tasks', item: 'task-25' },
    { group: 'work', page: 'Measurements', item: 'Attempt call-25' },
    { group: 'other', page: 'Measurements', item: 'Attempt call-25' },
  ])('pins the report when paging $group $page', async ({ group, page, item }) => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    try {
      const report = reportFixture()
      const task = { ...report.works[0].tasks![0], measurements: Array.from({ length: 26 }, (_, i) => ({
        ...report.works[0].tasks![0].measurements[0], id: `call-${i}`,
      })) }
      const work = { ...report.works[0],
        pullRequests: Array.from({ length: 26 }, (_, i) => ({ ...report.works[0].pullRequests![0], number: 101 + i })),
        tasks: Array.from({ length: 26 }, (_, i) => ({ ...task, taskUID: `task-${i}`, taskName: `task-${i}` })),
      }
      report.otherWork = [{ category: 'unassociated', explanation: 'Usage without a work reference', usage: report.summary, taskCount: 1 }]
      const requests: URL[] = []
      server.use(
        http.get('/api/v1/usage', ({ request }) => {
          requests.push(new URL(request.url))
          return HttpResponse.json(report)
        }),
        http.get('/api/v1/usage/work/work-0', () => HttpResponse.json({ work })),
        http.get('/api/v1/usage/other/unassociated', () => HttpResponse.json({ otherWork: {
          ...report.otherWork[0], page: { limit: 25, offset: 0, total: 1 }, tasks: [task],
        } })),
      )
      const user = userEvent.setup()
      render(<UsagePage />)
      await user.click(await screen.findByText(group === 'work' ? 'org/repo #1' : 'Chat and usage without a work reference'))
      await screen.findByText('task-0', { exact: false, selector: 'summary' })
      if (page === 'Measurements') {
        await user.click(screen.getByText('task-0', { exact: false, selector: 'summary' }))
        await screen.findByText('Attempt call-0')
      }
      await user.click(screen.getByRole('button', { name: `Next ${page}` }))
      await screen.findByText(item, { exact: page !== 'Tasks', ...(page === 'Tasks' ? { selector: 'summary' } : {}) })
      await waitFor(() => expect(requests).toHaveLength(2))
      expect(requests[1].searchParams.get('asOf')).toBe(report.selection.asOf)
      await act(async () => { await vi.advanceTimersByTimeAsync(60000) })
      expect(requests).toHaveLength(2)
      expect(screen.getByText(item, { exact: page !== 'Tasks', ...(page === 'Tasks' ? { selector: 'summary' } : {}) })).toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('bounds mounted Tasks and measurements within a large work request', async () => {
    const report = reportFixture()
    const task = report.works[0].tasks![0]
    const work = { ...report.works[0], tasks: Array.from({ length: 26 }, (_, i) => ({ ...task, taskUID: `task-${i}`, taskName: `task-${i}`,
      measurements: Array.from({ length: 26 }, (_, j) => ({ ...task.measurements[0], id: `call-${i}-${j}` })) })) }
    server.use(
      http.get('/api/v1/usage', () => HttpResponse.json(report)),
      http.get('/api/v1/usage/work/work-0', () => HttpResponse.json({ work })),
    )
    const user = userEvent.setup()
    render(<UsagePage />)
    await user.click(await screen.findByText('org/repo #1'))
    await screen.findByText('task-0', { exact: false, selector: 'summary' })
    expect(screen.queryByText('task-25', { exact: false, selector: 'summary' })).not.toBeInTheDocument()
    expect(screen.queryByText('Attempt call-0-0')).not.toBeInTheDocument()
    await user.click(screen.getByText('task-0', { exact: false, selector: 'summary' }))
    await screen.findByText('Attempt call-0-0')
    expect(screen.queryByText('Attempt call-0-25')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Next Measurements' }))
    expect(await screen.findByText('Attempt call-0-25')).toBeInTheDocument()
    expect(screen.queryByText('Attempt call-0-0')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Next Tasks' }))
    await screen.findByText('task-25', { exact: false, selector: 'summary' })
    expect(screen.queryByText('task-0', { exact: false, selector: 'summary' })).not.toBeInTheDocument()
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
    await user.type(screen.getByLabelText('Team namespace'), 'payments')
    await user.type(screen.getByLabelText('Repository'), 'org/repo')
    await user.type(screen.getByLabelText('Model used by the request'), 'served-model')
    await user.selectOptions(screen.getByLabelText('Work type'), 'pull_request')
    await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    await waitFor(() => expect(requests).toHaveLength(2))
    const params = requests[1].searchParams
    expect(params.get('namespace')).toBe('payments')
    expect(params.get('teams')).toBe('payments')
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
