import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'

const mockUseRepositoryScans = vi.fn()
const mockUseRunSecurityScan = vi.fn()

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

vi.mock('@/hooks/use-security', () => ({
  useRepositoryScans: () => mockUseRepositoryScans(),
  useRunSecurityScan: (...args: unknown[]) => mockUseRunSecurityScan(...args),
}))

import { render, screen, fireEvent } from '@/test/test-utils'
import { toast } from 'sonner'
import { ApiError } from '@/lib/api-client'
import type { RepositoryScan } from '@/schemas/security'
import { RepositoryList } from './repository-list'

function repository(overrides: Partial<RepositoryScan> = {}): RepositoryScan {
  return {
    metadata: { name: 'demo-repo', namespace: 'default' },
    spec: {
      repoURL: 'https://github.com/example/demo-repo',
      owner: 'example',
      repository: 'demo-repo',
      branch: 'main',
      analysisAgentRef: { name: 'security-agent' },
    },
    status: {
      phase: 'Ready',
      findingCounts: { total: 0 },
    },
    ...overrides,
  }
}

describe('RepositoryList', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-05-08T00:00:00Z'))
    mockUseRepositoryScans.mockReset()
    mockUseRunSecurityScan.mockReset()
    mockUseRunSecurityScan.mockReturnValue({ mutate: vi.fn(), isPending: false })
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('falls back to lastSuccessfulScanAt for repositories without lastScanAt', () => {
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      data: {
        items: [repository({ status: { phase: 'Ready', lastSuccessfulScanAt: '2026-05-06T00:00:00Z' } })],
      },
    })

    render(<RepositoryList />)

    expect(screen.getByText('2d ago')).toBeInTheDocument()
    expect(screen.queryByText('Never')).not.toBeInTheDocument()
  })

  it('prefers lastScanAt when both scan timestamps are present', () => {
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      data: {
        items: [
          repository({
            status: {
              phase: 'Ready',
              lastScanAt: '2026-05-07T23:59:30Z',
              lastSuccessfulScanAt: '2026-05-06T00:00:00Z',
            },
          }),
        ],
      },
    })

    render(<RepositoryList />)

    expect(screen.getByText('30s ago')).toBeInTheDocument()
  })

  it('titles each card with the RepositoryScan name so scans of one repository stay distinct', () => {
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      data: {
        items: [
          repository({ metadata: { name: 'demo-repo-main', namespace: 'default' } }),
          repository({ metadata: { name: 'demo-repo-api', namespace: 'default' } }),
        ],
      },
    })

    render(<RepositoryList />)

    expect(screen.getByRole('link', { name: 'demo-repo-main' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'demo-repo-api' })).toBeInTheDocument()
    expect(screen.getAllByText('https://github.com/example/demo-repo · main')).toHaveLength(2)
  })

  it('keeps the card icon full size beside a long repository name', () => {
    const longName = 'payments-platform-ledger-reconciliation-service-main-branch'
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      data: { items: [repository({ metadata: { name: longName, namespace: 'default' } })] },
    })

    render(<RepositoryList />)

    const icon = screen.getByRole('link', { name: longName }).parentElement?.querySelector('svg')
    expect(icon).toHaveClass('h-5', 'w-5', 'shrink-0')
  })

  it('names the Ready=False reason on an errored repository card', () => {
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      data: {
        items: [
          repository({
            status: {
              phase: 'Error',
              conditions: [{ type: 'Ready', status: 'False', reason: 'InvalidSchedule', message: 'expected exactly 5 fields, found 1: [hourly]' }],
            },
          }),
        ],
      },
    })

    render(<RepositoryList />)

    const reason = screen.getByText('InvalidSchedule')
    expect(reason).toHaveAttribute('title', 'expected exactly 5 fields, found 1: [hourly]')
    expect(screen.getByText('Error')).toBeInTheDocument()
  })

  it('adds no reason while a repository is only pending or scanning', () => {
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      data: {
        items: [
          repository({ status: { phase: 'Scanning', conditions: [{ type: 'Ready', status: 'False', reason: 'Scanning', message: 'Full scan is running' }] } }),
        ],
      },
    })

    render(<RepositoryList />)

    expect(screen.getAllByText('Scanning')).toHaveLength(1)
    expect(screen.queryByText('Full scan is running')).not.toBeInTheDocument()
  })

  it('keeps zero severity counts neutral and marks real ones', () => {
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      data: { items: [repository({ status: { phase: 'Ready', findingCounts: { total: 2, critical: 0, high: 2 } } })] },
    })

    render(<RepositoryList />)

    expect(screen.getByText('0 critical')).toHaveAttribute('data-variant', 'outline')
    expect(screen.getByText('2 high')).toHaveAttribute('data-variant', 'destructive')
  })

  it('confirms a started scan', () => {
    const mutate = vi.fn((_vars: unknown, options: { onSuccess: () => void }) => options.onSuccess())
    mockUseRunSecurityScan.mockReturnValue({ mutate, isPending: false })
    mockUseRepositoryScans.mockReturnValue({ isLoading: false, data: { items: [repository()] } })

    render(<RepositoryList />)
    fireEvent.click(screen.getByRole('button', { name: /scan now/i }))

    expect(mockUseRunSecurityScan).toHaveBeenCalledWith('demo-repo')
    expect(toast.success).toHaveBeenCalledWith('Manual scan of demo-repo started')
  })

  it('reports a refused scan with the server message', () => {
    const forbidden = new ApiError(403, '{"error":{"code":403,"message":"not authorized to scan"}}')
    const mutate = vi.fn((_vars: unknown, options: { onError: (error: unknown) => void }) => options.onError(forbidden))
    mockUseRunSecurityScan.mockReturnValue({ mutate, isPending: false })
    mockUseRepositoryScans.mockReturnValue({ isLoading: false, data: { items: [repository()] } })

    render(<RepositoryList />)
    fireEvent.click(screen.getByRole('button', { name: /scan now/i }))

    expect(toast.error).toHaveBeenCalledWith('Failed to start scan: not authorized to scan')
    expect(toast.success).not.toHaveBeenCalled()
  })
})
