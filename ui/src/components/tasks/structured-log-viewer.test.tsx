import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/tasks' }),
  }
})

vi.mock('@/hooks/use-task-logs', () => ({
  useTaskLogs: vi.fn(),
}))

import { useTaskLogs } from '@/hooks/use-task-logs'
import { StructuredLogViewer } from './structured-log-viewer'

const mockedUseTaskLogs = vi.mocked(useTaskLogs)

describe('StructuredLogViewer', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('shows no logs message when empty and not streaming', () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: [],
      isStreaming: false,
      isLive: false,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    expect(screen.getByText('No logs available yet.')).toBeInTheDocument()
  })

  it('renders log lines', () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['[INFO] Starting process', '[ERROR] Something failed', '[DEBUG] Trace info'],
      isStreaming: false,
      isLive: false,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    const logLines = screen.getAllByTestId('log-line')
    expect(logLines).toHaveLength(3)
    expect(screen.getByText(/Starting process/)).toBeInTheDocument()
    expect(screen.getByText(/Something failed/)).toBeInTheDocument()
    expect(screen.getByText('(3 lines)')).toBeInTheDocument()
  })

  it('filters logs based on search input', async () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['[INFO] Starting process', '[ERROR] Something failed', '[DEBUG] Trace info'],
      isStreaming: false,
      isLive: false,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)

    const searchInput = screen.getByPlaceholderText('Filter logs...')
    const { default: userEvent } = await import('@testing-library/user-event')
    const user = userEvent.setup()
    await user.type(searchInput, 'ERROR')

    const logLines = screen.getAllByTestId('log-line')
    expect(logLines).toHaveLength(1)
    expect(screen.getByText(/Something failed/)).toBeInTheDocument()
  })

  it('shows error state with retry button', () => {
    const refetchFn = vi.fn()
    mockedUseTaskLogs.mockReturnValue({
      logs: [],
      isStreaming: false,
      isLive: false,
      error: 'Failed to fetch logs: Internal Server Error',
      refetch: refetchFn,
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    expect(screen.getByText('Failed to fetch logs: Internal Server Error')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })

  it('shows streaming indicator', () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['[INFO] Running...'],
      isStreaming: true,
      isLive: false,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    const streaming = screen.getByText('Streaming')
    expect(streaming).toBeInTheDocument()
    // Liveness uses the reserved live token, not an ad-hoc pastel.
    expect(streaming.className).toContain('text-live')
  })

  it('shows live indicator for running tasks', () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['[INFO] Running...'],
      isStreaming: false,
      isLive: true,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" taskPhase="Running" />)
    const live = screen.getByText('Live')
    expect(live).toBeInTheDocument()
    expect(live.className).toContain('text-live')
    // The live badge carries a motion-safe pulsing dot (class includes the
    // motion-safe: variant prefix in the DOM).
    expect(live.querySelector('[class*="animate-pulse-live"]')).not.toBeNull()
  })
})
