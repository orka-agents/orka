import { describe, it, expect, beforeEach, vi } from 'vitest'
import { fireEvent, render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

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

  it('scrolls only the log container to the newest line, never the page', () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['line one', 'line two'],
      isStreaming: false,
      isLive: false,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })
    // scrollIntoView would also scroll every ancestor, moving the page.
    const originalScrollIntoView = Element.prototype.scrollIntoView
    const scrollIntoView = vi.fn()
    Element.prototype.scrollIntoView = scrollIntoView
    const scrollHeight = vi.spyOn(Element.prototype, 'scrollHeight', 'get').mockReturnValue(2000)
    try {
      render(<StructuredLogViewer taskId="task-1" />)
      const output = screen.getByRole('region', { name: 'Log output' })
      expect(output).toHaveAttribute('tabindex', '0')
      expect(output).toHaveClass('overflow-auto')
      expect(output).toContainElement(screen.getAllByTestId('log-line')[1])
      expect(output.scrollTop).toBe(2000)
      expect(scrollIntoView).not.toHaveBeenCalled()
    } finally {
      scrollHeight.mockRestore()
      Element.prototype.scrollIntoView = originalScrollIntoView
    }
  })

  it('wraps the header controls and lets the filter grow on narrow cards', () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['line one'],
      isStreaming: false,
      isLive: true,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })
    render(<StructuredLogViewer taskId="task-1" taskPhase="Running" />)
    const filter = screen.getByLabelText('Filter logs')
    expect(filter).toHaveClass('w-full', 'sm:w-48')
    expect(filter).not.toHaveClass('w-48')
    expect(filter.parentElement).toHaveClass('min-w-0', 'flex-1')
    const controls = filter.parentElement!.parentElement!
    expect(controls).toHaveClass('w-full', 'sm:w-auto')
    expect(controls).toContainElement(screen.getByRole('button', { name: 'Clear logs' }))
    expect(controls.parentElement).toHaveClass('flex-wrap')
  })

  it('holds the lines being read while unpinned and offers a jump to the latest', async () => {
    const user = userEvent.setup()
    const hookResult = (logs: string[]) => ({
      logs,
      isStreaming: false,
      isLive: true,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })
    const lineTexts = () => screen.getAllByTestId('log-line').map((line) => line.textContent)
    mockedUseTaskLogs.mockReturnValue(hookResult(['first', 'second']))
    const scrollHeight = vi.spyOn(Element.prototype, 'scrollHeight', 'get').mockReturnValue(2000)
    try {
      const { rerender } = render(<StructuredLogViewer taskId="task-1" taskPhase="Running" />)
      const output = screen.getByRole('region', { name: 'Log output' })
      // The reader scrolls up, which unpins the view.
      output.scrollTop = 100
      fireEvent.scroll(output)
      expect(screen.getByRole('button', { name: 'Pin logs to bottom' })).not.toHaveClass('bg-primary')

      // The next poll returns a slid tail window; the rows on screen stay put.
      mockedUseTaskLogs.mockReturnValue(hookResult(['second', 'third']))
      rerender(<StructuredLogViewer taskId="task-1" taskPhase="Running" />)
      expect(lineTexts()).toEqual(['1first', '2second'])

      await user.click(screen.getByRole('button', { name: /jump to latest/i }))
      expect(lineTexts()).toEqual(['1second', '2third'])
      expect(screen.queryByRole('button', { name: /jump to latest/i })).not.toBeInTheDocument()
    } finally {
      scrollHeight.mockRestore()
    }
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

  it('keeps original row numbers and shows loaded counts while filtering', async () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['[INFO] Starting', '[INFO] Ready', '[ERROR] Request failed', '[ERROR] Request failed'],
      isStreaming: false,
      isLive: false,
      error: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    expect(screen.getByText('(4 lines)')).toBeInTheDocument()

    const searchInput = screen.getByPlaceholderText('Filter logs...')
    const { default: userEvent } = await import('@testing-library/user-event')
    const user = userEvent.setup()
    await user.type(searchInput, 'error')

    const logLines = screen.getAllByTestId('log-line')
    expect(logLines).toHaveLength(2)
    expect(screen.getByText('(2 of 4 loaded lines)')).toBeInTheDocument()
    // Duplicate messages stay separate rows with their buffer positions.
    expect(logLines[0]).toHaveTextContent('3')
    expect(logLines[1]).toHaveTextContent('4')
    expect(logLines[0]).toHaveTextContent('Request failed')
    expect(logLines[1]).toHaveTextContent('Request failed')
  })

  it('shows a no-matches message and clearing the filter restores all rows', async () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: ['[INFO] Starting', '[INFO] Ready', '[ERROR] Request failed', '[ERROR] Request failed'],
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
    await user.type(searchInput, 'zzz')

    expect(screen.getByTestId('log-no-matches')).toHaveTextContent('No matching lines for "zzz".')
    expect(screen.queryByTestId('log-line')).toBeNull()

    await user.click(screen.getByRole('button', { name: /clear filter/i }))

    expect(screen.queryByTestId('log-no-matches')).toBeNull()
    expect(screen.getAllByTestId('log-line')).toHaveLength(4)
    expect(screen.getByText('(4 lines)')).toBeInTheDocument()
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

  it('explains a task with no pod logs instead of showing an error with Retry', async () => {
    const refetchFn = vi.fn()
    mockedUseTaskLogs.mockReturnValue({
      logs: [],
      isStreaming: false,
      isLive: false,
      error: null,
      unavailable: 'no pods found for task job',
      refetch: refetchFn,
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    expect(screen.getByText('No pod logs for this task')).toBeInTheDocument()
    expect(screen.getByText(/No pods found for task job\. Logs appear once the task's pod starts/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument()
    screen.getByRole('button', { name: /check again/i }).click()
    expect(refetchFn).toHaveBeenCalled()
  })

  it('gives line numbers a fixed width so text does not shift as numbers grow', () => {
    mockedUseTaskLogs.mockReturnValue({
      logs: Array.from({ length: 120 }, (_, i) => (i === 3 ? '' : `line ${i + 1}`)),
      isStreaming: false,
      isLive: false,
      error: null,
      unavailable: null,
      refetch: vi.fn(),
      clear: vi.fn(),
    })

    render(<StructuredLogViewer taskId="task-1" />)
    const rows = screen.getAllByTestId('log-line')
    // Blank lines are rendered as rows too.
    expect(rows).toHaveLength(120)
    const firstNumber = rows[0].querySelector('span')!
    const lastNumber = rows[119].querySelector('span')!
    expect(firstNumber).toHaveStyle({ width: '3ch' })
    expect(lastNumber).toHaveStyle({ width: '3ch' })
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
