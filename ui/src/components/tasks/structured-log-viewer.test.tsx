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
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    expect(screen.getByText('● Streaming')).toBeInTheDocument()
  })
})
