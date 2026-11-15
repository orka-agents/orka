import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'

const mockUseRepositoryMonitors = vi.fn()
const mockUseRunRepositoryMonitor = vi.fn()

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => {
      const { params, ...anchorProps } = props
      void params
      return <a href={to} {...anchorProps}>{children}</a>
    },
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@/hooks/use-monitors', () => ({
  useRepositoryMonitors: () => mockUseRepositoryMonitors(),
  useRunRepositoryMonitor: (...args: unknown[]) => mockUseRunRepositoryMonitor(...args),
}))

import { render, screen } from '@/test/test-utils'
import { fireEvent } from '@testing-library/react'
import { toast } from 'sonner'
import { ApiError } from '@/lib/api-client'
import type { RepositoryMonitor } from '@/schemas/monitor'
import { RepositoryMonitorList } from './repository-monitor-list'

function monitor(overrides: Partial<RepositoryMonitor> = {}): RepositoryMonitor {
  return {
    metadata: { name: 'repo-monitor', namespace: 'default' },
    spec: {
      repoURL: 'https://github.com/orka-agents/orka',
      owner: 'orka-agents',
      repository: 'orka',
      branch: 'main',
      schedule: '*/15 * * * *',
    },
    status: {
      phase: 'Ready',
      pendingReviews: 2,
      activeRepairs: 1,
      openPullRequests: 4,
      lastRunTime: '2026-05-07T23:59:30Z',
    },
    ...overrides,
  }
}

describe('RepositoryMonitorList', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-05-08T00:00:00Z'))
    mockUseRepositoryMonitors.mockReset()
    mockUseRunRepositoryMonitor.mockReset()
    mockUseRunRepositoryMonitor.mockReturnValue({ mutate: vi.fn(), isPending: false })
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
  })

  afterEach(() => {
    vi.useRealTimers()
  })


  it('links to the create form from the empty state', () => {
    mockUseRepositoryMonitors.mockReturnValue({
      isLoading: false,
      data: { items: [] },
    })

    render(<RepositoryMonitorList />)

    expect(screen.getByText('No repository monitors configured.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /New Monitor/i })).toHaveAttribute('href', '/monitors/create/new')
    expect(screen.getByRole('link', { name: /Create repository monitor/i })).toHaveAttribute('href', '/monitors/create/new')
  })

  it('renders repository monitor status and last run time', () => {
    mockUseRepositoryMonitors.mockReturnValue({
      isLoading: false,
      data: { items: [monitor()] },
    })

    render(<RepositoryMonitorList />)

    expect(screen.getByRole('link', { name: 'repo-monitor' })).toBeInTheDocument()
    expect(screen.getByText('orka-agents/orka · main · */15 * * * *')).toBeInTheDocument()
    expect(screen.getByText(/Pending reviews:/)).toHaveTextContent('Pending reviews: 2')
    expect(screen.getByText(/Active repairs:/)).toHaveTextContent('Active repairs: 1')
    expect(screen.getByText('30s ago')).toBeInTheDocument()
  })

  it('derives the repository display name for CRD-created monitors', () => {
    mockUseRepositoryMonitors.mockReturnValue({
      isLoading: false,
      data: { items: [monitor({ spec: { repoURL: 'git@github.com:orka-agents/orka.git' } })] },
    })

    render(<RepositoryMonitorList />)

    expect(screen.getByText('orka-agents/orka · main · manual')).toBeInTheDocument()
    expect(screen.queryByText(/undefined/)).not.toBeInTheDocument()
  })

  it('titles each card with the monitor name so monitors of one repository stay distinct', () => {
    mockUseRepositoryMonitors.mockReturnValue({
      isLoading: false,
      data: {
        items: [
          monitor({ metadata: { name: 'orka-main', namespace: 'default' } }),
          monitor({ metadata: { name: 'orka-nightly', namespace: 'default' } }),
        ],
      },
    })

    render(<RepositoryMonitorList />)

    expect(screen.getByRole('link', { name: 'orka-main' })).toHaveAttribute('href', '/monitors/$monitorId')
    expect(screen.getByRole('link', { name: 'orka-nightly' })).toBeInTheDocument()
    expect(screen.getAllByText('orka-agents/orka · main · */15 * * * *')).toHaveLength(2)
  })

  it('keeps the card icon full size beside a long monitor name', () => {
    const longName = 'orka-pull-request-review-monitor-for-the-main-branch'
    mockUseRepositoryMonitors.mockReturnValue({
      isLoading: false,
      data: { items: [monitor({ metadata: { name: longName, namespace: 'default' } })] },
    })

    render(<RepositoryMonitorList />)

    const icon = screen.getByRole('link', { name: longName }).parentElement?.querySelector('svg')
    expect(icon).toHaveClass('h-5', 'w-5', 'shrink-0')
  })

  it('names the Ready=False reason on an errored monitor card', () => {
    const errored = monitor({
      status: {
        phase: 'Error',
        conditions: [{ type: 'Ready', status: 'False', reason: 'InvalidSchedule', message: 'end of range (61) above maximum (59): 61' }],
      } as RepositoryMonitor['status'],
    })
    mockUseRepositoryMonitors.mockReturnValue({ isLoading: false, data: { items: [errored] } })

    render(<RepositoryMonitorList />)

    expect(screen.getByText('InvalidSchedule')).toHaveAttribute('title', 'end of range (61) above maximum (59): 61')
  })

  it('adds no reason to a suspended monitor card', () => {
    const suspended = monitor({
      status: {
        phase: 'Suspended',
        conditions: [{ type: 'Ready', status: 'False', reason: 'Suspended', message: 'Repository monitor scheduled runs are suspended' }],
      } as RepositoryMonitor['status'],
    })
    mockUseRepositoryMonitors.mockReturnValue({ isLoading: false, data: { items: [suspended] } })

    render(<RepositoryMonitorList />)

    expect(screen.getAllByText('Suspended')).toHaveLength(1)
  })

  it('runs a monitor from the list action', () => {
    const mutate = vi.fn()
    mockUseRunRepositoryMonitor.mockReturnValue({ mutate, isPending: false })
    mockUseRepositoryMonitors.mockReturnValue({
      isLoading: false,
      data: { items: [monitor()] },
    })

    render(<RepositoryMonitorList />)

    fireEvent.click(screen.getByRole('button', { name: /run/i }))

    expect(mutate).toHaveBeenCalledTimes(1)
    expect(mockUseRunRepositoryMonitor).toHaveBeenCalledWith('repo-monitor')
  })

  it('confirms a started run', () => {
    const mutate = vi.fn((_vars: unknown, options: { onSuccess: () => void }) => options.onSuccess())
    mockUseRunRepositoryMonitor.mockReturnValue({ mutate, isPending: false })
    mockUseRepositoryMonitors.mockReturnValue({ isLoading: false, data: { items: [monitor()] } })

    render(<RepositoryMonitorList />)
    fireEvent.click(screen.getByRole('button', { name: /run/i }))

    expect(toast.success).toHaveBeenCalledWith('Run of repo-monitor started')
  })

  it('reports a refused run with the server message', () => {
    const forbidden = new ApiError(403, '{"error":{"code":403,"message":"not authorized to run monitors"}}')
    const mutate = vi.fn((_vars: unknown, options: { onError: (error: unknown) => void }) => options.onError(forbidden))
    mockUseRunRepositoryMonitor.mockReturnValue({ mutate, isPending: false })
    mockUseRepositoryMonitors.mockReturnValue({ isLoading: false, data: { items: [monitor()] } })

    render(<RepositoryMonitorList />)
    fireEvent.click(screen.getByRole('button', { name: /run/i }))

    expect(toast.error).toHaveBeenCalledWith('Failed to start monitor run: not authorized to run monitors')
    expect(toast.success).not.toHaveBeenCalled()
  })
})
