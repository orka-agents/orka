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

vi.mock('@/hooks/use-monitors', () => ({
  useRepositoryMonitors: () => mockUseRepositoryMonitors(),
  useRunRepositoryMonitor: (...args: unknown[]) => mockUseRunRepositoryMonitor(...args),
}))

import { render, screen } from '@/test/test-utils'
import { fireEvent } from '@testing-library/react'
import type { RepositoryMonitor } from '@/schemas/monitor'
import { RepositoryMonitorList } from './repository-monitor-list'

function monitor(overrides: Partial<RepositoryMonitor> = {}): RepositoryMonitor {
  return {
    metadata: { name: 'repo-monitor', namespace: 'default' },
    spec: {
      repoURL: 'https://github.com/sozercan/orka',
      owner: 'sozercan',
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

    expect(screen.getByText('sozercan/orka')).toBeInTheDocument()
    expect(screen.getByText(/Pending reviews:/)).toHaveTextContent('Pending reviews: 2')
    expect(screen.getByText(/Active repairs:/)).toHaveTextContent('Active repairs: 1')
    expect(screen.getByText('30s ago')).toBeInTheDocument()
  })

  it('derives the repository display name for CRD-created monitors', () => {
    mockUseRepositoryMonitors.mockReturnValue({
      isLoading: false,
      data: { items: [monitor({ spec: { repoURL: 'git@github.com:sozercan/orka.git' } })] },
    })

    render(<RepositoryMonitorList />)

    expect(screen.getByText('sozercan/orka')).toBeInTheDocument()
    expect(screen.queryByText(/undefined/)).not.toBeInTheDocument()
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
})
