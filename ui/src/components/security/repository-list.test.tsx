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

vi.mock('@/hooks/use-security', () => ({
  useRepositoryScans: () => mockUseRepositoryScans(),
  useRunSecurityScan: (...args: unknown[]) => mockUseRunSecurityScan(...args),
}))

import { render, screen } from '@/test/test-utils'
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
})
